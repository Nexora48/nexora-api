// Package attendance logs when daily help and society staff come and go, from the gate scan or manual office entries,
// and owns the staff registry. Helpers themselves are managed by the helper package; this package only reads them.
package attendance

import (
	"context"
	"errors"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/helper"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/pass"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

const lateGrace = 15 * time.Minute

var (
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "no one matches that pass")
	ErrAlreadyInside = httpx.NewError(http.StatusConflict, "ALREADY_INSIDE", "this person is already checked in")
	ErrNotInside     = httpx.NewError(http.StatusConflict, "NOT_INSIDE", "this person is not checked in")
	ErrBlocked       = httpx.NewError(http.StatusForbidden, "BLOCKED", "this person is not allowed entry; please call the society office")
	ErrAmbiguous     = httpx.NewError(http.StatusConflict, "AMBIGUOUS_CODE", "that code matches more than one person; scan the QR instead")
	ErrBadPass       = httpx.NewError(http.StatusBadRequest, "BAD_PASS", "enter the 6-digit code or scan a helper or staff QR")
)

func invalid(msg string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

type Service struct {
	pool     *pgxpool.Pool
	audit    *audit.Logger
	files    *attachment.Service
	note     *notify.Service
	hub      *realtime.Hub
	helperQR *pass.Signer
	staffQR  *pass.Signer
	nowFn    func() time.Time
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, files *attachment.Service, note *notify.Service, hub *realtime.Hub, secret string) *Service {
	return &Service{pool: pool, audit: a, files: files, note: note, hub: hub, helperQR: helper.NewSigner(secret), staffQR: NewStaffSigner(secret), nowFn: time.Now}
}

// NewStaffSigner is the staff QR signer.
func NewStaffSigner(secret string) *pass.Signer { return pass.NewSigner(secret, "staff-qr", "NXS1:") }

func (s *Service) log(ctx context.Context, p authctx.Principal, action, entity string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: entity, EntityID: id.String(), Meta: meta})
}

func (s *Service) loc(ctx context.Context, soc uuid.UUID) *time.Location {
	var tz string
	_ = s.pool.QueryRow(ctx, `SELECT timezone FROM societies WHERE id=$1`, soc).Scan(&tz)
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+1800)
}

// Subject is who a pass belongs to, as the guard sees it: never the phone number or ID digits.
type Subject struct {
	Type     string     `json:"subject_type"` // helper | staff
	ID       uuid.UUID  `json:"subject_id"`
	Name     string     `json:"name"`
	Role     string     `json:"role"` // maid, cook… or housekeeping, gardener…
	Status   string     `json:"status"`
	HasPhoto bool       `json:"has_photo"`
	Flats    []string   `json:"flats,omitempty"`
	Shift    *string    `json:"shift,omitempty"`
	Inside   bool       `json:"inside"`
	InAt     *time.Time `json:"in_at,omitempty"`
	Warning  string     `json:"warning,omitempty"` // PENDING | OFF_HOURS
}

func clockMin(t time.Time) int { return t.Hour()*60 + t.Minute() }

func parseMin(v string) (int, bool) {
	if len(v) >= 5 {
		v = v[:5]
	}
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return clockMin(t), true
}

type sched struct {
	days     []int16
	from, to *string
}

func (sc sched) matches(at time.Time) bool {
	if len(sc.days) > 0 {
		ok := false
		for _, d := range sc.days {
			ok = ok || int(d) == int(at.Weekday())
		}
		if !ok {
			return false
		}
	}
	if sc.from != nil && sc.to != nil {
		f, ok1 := parseMin(*sc.from)
		t, ok2 := parseMin(*sc.to)
		if ok1 && ok2 && (clockMin(at) < f || clockMin(at) > t) {
			return false
		}
	}
	return true
}

type resolved struct {
	Subject
	flats  []uuid.UUID
	scheds []sched
	// staff
	shiftFrom, shiftTo string
	workDays           []int16
	active             bool
}

func (s *Service) loadHelper(ctx context.Context, soc, id uuid.UUID) (*resolved, error) {
	r := &resolved{Subject: Subject{Type: "helper", ID: id}}
	err := s.pool.QueryRow(ctx, `SELECT full_name, kind, status, EXISTS (SELECT 1 FROM attachments a WHERE a.owner_type='helper' AND a.owner_id=helpers.id) FROM helpers WHERE id=$1 AND society_id=$2`, id, soc).
		Scan(&r.Name, &r.Role, &r.Status, &r.HasPhoto)
	if err != nil {
		return nil, ErrNotFound
	}
	r.active = r.Status != "suspended" && r.Status != "blacklisted"
	rows, err := s.pool.Query(ctx, `SELECT hf.unit_id, b.code || '-' || u.number, hf.days, hf.window_from::text, hf.window_to::text
		FROM helper_flats hf JOIN units u ON u.id=hf.unit_id JOIN blocks b ON b.id=u.block_id WHERE hf.helper_id=$1 ORDER BY b.code, u.number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid uuid.UUID
		var label string
		var sc sched
		if err := rows.Scan(&uid, &label, &sc.days, &sc.from, &sc.to); err != nil {
			return nil, err
		}
		r.flats = append(r.flats, uid)
		r.Flats = append(r.Flats, label)
		r.scheds = append(r.scheds, sc)
	}
	return r, rows.Err()
}

func (s *Service) loadStaff(ctx context.Context, soc, id uuid.UUID) (*resolved, error) {
	r := &resolved{Subject: Subject{Type: "staff", ID: id}}
	err := s.pool.QueryRow(ctx, `SELECT full_name, role, active, shift_from::text, shift_to::text, work_days, EXISTS (SELECT 1 FROM attachments a WHERE a.owner_type='staff' AND a.owner_id=society_staff.id)
		FROM society_staff WHERE id=$1 AND society_id=$2`, id, soc).Scan(&r.Name, &r.Role, &r.active, &r.shiftFrom, &r.shiftTo, &r.workDays, &r.HasPhoto)
	if err != nil {
		return nil, ErrNotFound
	}
	r.Status = "active"
	if !r.active {
		r.Status = "inactive"
	}
	sh := r.shiftFrom[:5] + "-" + r.shiftTo[:5]
	r.Shift = &sh
	return r, nil
}

func (s *Service) load(ctx context.Context, soc uuid.UUID, typ string, id uuid.UUID) (*resolved, error) {
	var r *resolved
	var err error
	switch typ {
	case "helper":
		r, err = s.loadHelper(ctx, soc, id)
	case "staff":
		r, err = s.loadStaff(ctx, soc, id)
	default:
		return nil, invalid("subject_type must be helper or staff")
	}
	if err != nil {
		return nil, err
	}
	var in time.Time
	if e := s.pool.QueryRow(ctx, `SELECT in_at FROM attendance_logs WHERE subject_type=$1 AND subject_id=$2 AND out_at IS NULL`, typ, id).Scan(&in); e == nil {
		r.Inside, r.InAt = true, &in
	}
	return r, nil
}

func (r *resolved) offHours(at time.Time) bool {
	if r.Type == "staff" {
		worked := false
		for _, d := range r.workDays {
			worked = worked || int(d) == int(at.Weekday())
		}
		f, _ := parseMin(r.shiftFrom)
		t, _ := parseMin(r.shiftTo)
		return !worked || clockMin(at) < f-60 || clockMin(at) > t
	}
	for _, sc := range r.scheds {
		if sc.matches(at) {
			return false
		}
	}
	return true
}

// late is true when the person arrives more than the grace period after the expected start.
func (r *resolved) late(at time.Time) bool {
	if r.Type == "staff" {
		worked := false
		for _, d := range r.workDays {
			worked = worked || int(d) == int(at.Weekday())
		}
		f, ok := parseMin(r.shiftFrom)
		return worked && ok && clockMin(at) > f+int(lateGrace.Minutes())
	}
	start := -1
	for _, sc := range r.scheds {
		if sc.from == nil || !sc.matches(at) {
			continue
		}
		if f, ok := parseMin(*sc.from); ok && (start < 0 || f < start) {
			start = f
		}
	}
	return start >= 0 && clockMin(at) > start+int(lateGrace.Minutes())
}

func (s *Service) decorate(r *resolved, at time.Time) {
	switch {
	case r.Type == "helper" && r.Status == "pending":
		r.Warning = "PENDING"
	case !r.Inside && r.offHours(at):
		r.Warning = "OFF_HOURS"
	}
}

// Resolve identifies who a scanned QR payload or typed 6-digit code belongs to.
func (s *Service) Resolve(ctx context.Context, p authctx.Principal, payload, code string) (*Subject, error) {
	typ, id, err := s.identify(ctx, p.SocietyID, payload, code)
	if err != nil {
		return nil, err
	}
	r, err := s.load(ctx, p.SocietyID, typ, id)
	if err != nil {
		return nil, err
	}
	s.decorate(r, s.nowFn().In(s.loc(ctx, p.SocietyID)))
	return &r.Subject, nil
}

func (s *Service) identify(ctx context.Context, soc uuid.UUID, payload, code string) (string, uuid.UUID, error) {
	if payload = strings.TrimSpace(payload); payload != "" {
		if id, ok := s.helperQR.Parse(payload); ok {
			return "helper", id, nil
		}
		if id, ok := s.staffQR.Parse(payload); ok {
			return "staff", id, nil
		}
		return "", uuid.Nil, ErrBadPass
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return "", uuid.Nil, ErrBadPass
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return "", uuid.Nil, ErrBadPass
		}
	}
	var hid, sid *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT id FROM helpers WHERE society_id=$1 AND pass_code=$2`, soc, code).Scan(&hid)
	_ = s.pool.QueryRow(ctx, `SELECT id FROM society_staff WHERE society_id=$1 AND pass_code=$2 AND active`, soc, code).Scan(&sid)
	switch {
	case hid != nil && sid != nil:
		return "", uuid.Nil, ErrAmbiguous
	case hid != nil:
		return "helper", *hid, nil
	case sid != nil:
		return "staff", *sid, nil
	}
	return "", uuid.Nil, ErrNotFound
}

type Record struct {
	ID          uuid.UUID  `json:"id"`
	SubjectType string     `json:"subject_type"`
	SubjectID   uuid.UUID  `json:"subject_id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	InAt        time.Time  `json:"in_at"`
	OutAt       *time.Time `json:"out_at,omitempty"`
	Source      string     `json:"source"`
	Late        bool       `json:"late"`
	OffHours    bool       `json:"off_hours"`
	Note        *string    `json:"note,omitempty"`
	Warning     string     `json:"warning,omitempty"`
}

func (s *Service) record(ctx context.Context, soc, id uuid.UUID) (*Record, error) {
	var r Record
	err := s.pool.QueryRow(ctx, `SELECT a.id, a.subject_type, a.subject_id, COALESCE(h.full_name, st.full_name), COALESCE(h.kind, st.role), a.in_at, a.out_at, a.source, a.late, a.off_hours, a.note
		FROM attendance_logs a LEFT JOIN helpers h ON a.subject_type='helper' AND h.id=a.subject_id LEFT JOIN society_staff st ON a.subject_type='staff' AND st.id=a.subject_id
		WHERE a.id=$1 AND a.society_id=$2`, id, soc).Scan(&r.ID, &r.SubjectType, &r.SubjectID, &r.Name, &r.Role, &r.InAt, &r.OutAt, &r.Source, &r.Late, &r.OffHours, &r.Note)
	return &r, err
}

// clampAt accepts the time an offline device recorded, but never from the future or older than a day.
func (s *Service) clampAt(at *time.Time) time.Time {
	now := s.nowFn()
	if at == nil || at.IsZero() || at.After(now) {
		return now
	}
	if at.Before(now.Add(-24 * time.Hour)) {
		return now.Add(-24 * time.Hour)
	}
	return *at
}

// CheckIn records an arrival.
func (s *Service) CheckIn(ctx context.Context, p authctx.Principal, typ string, id uuid.UUID, gate *uuid.UUID, at *time.Time) (*Record, error) {
	r, err := s.load(ctx, p.SocietyID, typ, id)
	if err != nil {
		return nil, err
	}
	if !r.active {
		return nil, ErrBlocked
	}
	when := s.clampAt(at)
	local := when.In(s.loc(ctx, p.SocietyID))
	var rid uuid.UUID
	err = s.pool.QueryRow(ctx, `INSERT INTO attendance_logs (society_id, subject_type, subject_id, gate_id, in_at, in_by, late, off_hours) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		p.SocietyID, typ, id, gate, when, p.MembershipID, r.late(local), r.offHours(local)).Scan(&rid)
	if database.IsUnique(err, "attendance_open_uq") {
		return nil, ErrAlreadyInside
	}
	if err != nil {
		return nil, err
	}
	rec, err := s.record(ctx, p.SocietyID, rid)
	if err != nil {
		return nil, err
	}
	if r.Type == "helper" && r.Status == "pending" {
		rec.Warning = "PENDING"
	}
	s.announce(ctx, p.SocietyID, r, rec, "in")
	return rec, nil
}

// CheckOut records a departure.
func (s *Service) CheckOut(ctx context.Context, p authctx.Principal, typ string, id uuid.UUID, at *time.Time) (*Record, error) {
	r, err := s.load(ctx, p.SocietyID, typ, id)
	if err != nil {
		return nil, err
	}
	when := s.clampAt(at)
	var rid uuid.UUID
	err = s.pool.QueryRow(ctx, `UPDATE attendance_logs SET out_at=GREATEST($4::timestamptz, in_at), out_by=$5 WHERE subject_type=$1 AND subject_id=$2 AND society_id=$3 AND out_at IS NULL RETURNING id`,
		typ, id, p.SocietyID, when, p.MembershipID).Scan(&rid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotInside
	}
	if err != nil {
		return nil, err
	}
	rec, err := s.record(ctx, p.SocietyID, rid)
	if err != nil {
		return nil, err
	}
	s.announce(ctx, p.SocietyID, r, rec, "out")
	return rec, nil
}

// announce tells the employing flats (when they opted in), the gate and the office.
func (s *Service) announce(ctx context.Context, soc uuid.UUID, r *resolved, rec *Record, dir string) {
	if s.hub != nil {
		ev := realtime.Event{Type: "attendance.updated", SocietyID: soc, Guards: true, Roles: []string{"society_admin", "committee"},
			Data: map[string]any{"subject_type": r.Type, "subject_id": r.ID, "inside": dir == "in"}}
		s.hub.Publish(ctx, ev)
		for _, u := range r.flats {
			u := u
			s.hub.Publish(ctx, realtime.Event{Type: "attendance.updated", SocietyID: soc, Unit: &u, Data: ev.Data})
		}
	}
	if s.note == nil || r.Type != "helper" {
		return
	}
	col, typ, verb := "notify_entry", "helper.arrived", "arrived"
	if dir == "out" {
		col, typ, verb = "notify_exit", "helper.left", "left"
	}
	rows, err := s.pool.Query(ctx, `SELECT unit_id FROM helper_flats WHERE helper_id=$1 AND `+col, r.ID)
	if err != nil {
		return
	}
	var users []uuid.UUID
	var units []uuid.UUID
	for rows.Next() {
		var u uuid.UUID
		if rows.Scan(&u) == nil {
			units = append(units, u)
		}
	}
	rows.Close()
	for _, u := range units {
		users = append(users, s.note.UnitMembers(ctx, soc, u)...)
	}
	if len(users) == 0 {
		return
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: typ, Title: r.Name + " " + verb, Body: "Your " + r.Role + " " + verb + " at the gate",
		Data: map[string]any{"type": "helper", "helper_id": r.ID}, Priority: notify.PrioritySilent})
}

// Inside lists everyone currently checked in (guard-safe: no phone numbers).
func (s *Service) Inside(ctx context.Context, p authctx.Principal) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id FROM attendance_logs a WHERE a.society_id=$1 AND a.out_at IS NULL ORDER BY a.in_at DESC LIMIT 500`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := []Record{}
	for _, id := range ids {
		if r, err := s.record(ctx, p.SocietyID, id); err == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// SyncEntry is one person in the guard's offline cache.
type SyncEntry struct {
	Subject
	CodeHash string `json:"code_hash"`
}

// Sync lists everybody a guard may need to check in while offline (code hashed, no phone numbers).
func (s *Service) Sync(ctx context.Context, p authctx.Principal) ([]SyncEntry, error) {
	out := []SyncEntry{}
	rows, err := s.pool.Query(ctx, `SELECT 'helper', id, full_name, kind, status, pass_code FROM helpers WHERE society_id=$1 AND status IN ('pending','verified')
		UNION ALL SELECT 'staff', id, full_name, role, 'active', pass_code FROM society_staff WHERE society_id=$1 AND active LIMIT 3000`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e SyncEntry
		var code string
		if err := rows.Scan(&e.Type, &e.ID, &e.Name, &e.Role, &e.Status, &code); err != nil {
			return nil, err
		}
		e.CodeHash = pass.CodeHash(p.SocietyID, code)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Photo opens the photo of a helper or staff member for the gate.
func (s *Service) Photo(ctx context.Context, p authctx.Principal, typ string, id uuid.UUID) ([]byte, *attachment.File, error) {
	owner := attachment.OwnerHelper
	if typ == "staff" {
		owner = attachment.OwnerStaff
	} else if typ != "helper" {
		return nil, nil, ErrNotFound
	}
	fs, err := s.files.List(ctx, p.SocietyID, owner, id)
	if err != nil {
		return nil, nil, err
	}
	if len(fs) == 0 {
		return nil, nil, attachment.ErrNotFound
	}
	return s.files.Open(ctx, p.SocietyID, owner, id, fs[0].ID)
}

// Sweep closes records left open past local midnight: a forgotten check-out must not keep someone "inside" forever.
func (s *Service) Sweep(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE attendance_logs a SET out_at = LEAST(now(), ((date_trunc('day', a.in_at AT TIME ZONE so.timezone) + interval '1 day') AT TIME ZONE so.timezone) - interval '1 second'), source='auto_closed'
		FROM societies so WHERE so.id=a.society_id AND a.out_at IS NULL AND a.in_at < (date_trunc('day', now() AT TIME ZONE so.timezone) AT TIME ZONE so.timezone)`)
	return tag.RowsAffected(), err
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("attendance")
			_, _ = s.Sweep(ctx)
		}
	}
}
