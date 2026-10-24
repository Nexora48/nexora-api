// Package helper manages daily help (maids, cooks, drivers…): one record per person per society, linked to the
// flats they work for, each with a permanent QR pass and a regenerable 6-digit code for the guard.
package helper

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/pass"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/resident"
)

const maxPerFlat = 10

var (
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "helper not found")
	ErrNoUnit        = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrAlreadyLinked = httpx.NewError(http.StatusConflict, "ALREADY_LINKED", "this person is already on your flat's list")
	ErrBlacklisted   = httpx.NewError(http.StatusForbidden, "BLACKLISTED", "this person cannot be registered; please contact the society office")
	ErrFlatLimit     = httpx.NewError(http.StatusUnprocessableEntity, "FLAT_LIMIT", "your flat has reached the limit of registered helpers")
	ErrInvalidState  = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this helper cannot be changed that way")
	idRe             = regexp.MustCompile(`^[A-Za-z0-9]{4}$`)
	kinds            = map[string]bool{"maid": true, "cook": true, "driver": true, "nanny": true, "other": true}
)

func invalid(msg string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	files *attachment.Service
	note  *notify.Service
	hub   *realtime.Hub
	qr    *pass.Signer
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, files *attachment.Service, note *notify.Service, hub *realtime.Hub, secret string) *Service {
	return &Service{pool: pool, audit: a, files: files, note: note, hub: hub, qr: NewSigner(secret)}
}

// NewSigner is the helper QR signer; attendance uses the same one to read scans.
func NewSigner(secret string) *pass.Signer { return pass.NewSigner(secret, "helper-qr", "NXH1:") }

type Flat struct {
	UnitID      uuid.UUID `json:"unit_id"`
	Flat        string    `json:"flat"`
	Days        []int     `json:"days"`
	WindowFrom  *string   `json:"window_from,omitempty"`
	WindowTo    *string   `json:"window_to,omitempty"`
	NotifyEntry bool      `json:"notify_entry"`
	NotifyExit  bool      `json:"notify_exit"`
}

type Today struct {
	Inside bool       `json:"inside"`
	InAt   *time.Time `json:"in_at,omitempty"`
	OutAt  *time.Time `json:"out_at,omitempty"`
}

type Helper struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"full_name"`
	Phone        string    `json:"phone"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	StatusReason *string   `json:"status_reason,omitempty"`
	IDLast4      *string   `json:"id_last4,omitempty"`
	HasPhoto     bool      `json:"has_photo"`
	Rating       *float64  `json:"rating,omitempty"`
	RatingCount  int       `json:"rating_count"`
	MyRating     *int      `json:"my_rating,omitempty"`
	MyComment    *string   `json:"my_comment,omitempty"`
	Flats        []Flat    `json:"flats"`
	Today        *Today    `json:"today,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "helper", EntityID: id.String(), Meta: meta})
}

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) *uuid.UUID {
	var u *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	return u
}

const helperSelect = `SELECT h.id, h.full_name, h.phone, h.kind, h.status, h.status_reason, h.id_last4, h.created_at,
	EXISTS (SELECT 1 FROM attachments a WHERE a.owner_type='helper' AND a.owner_id=h.id),
	(SELECT avg(rating)::float8 FROM helper_ratings r WHERE r.helper_id=h.id), (SELECT count(*) FROM helper_ratings r WHERE r.helper_id=h.id)
	FROM helpers h `

func scanHelper(row pgx.Row) (*Helper, error) {
	var h Helper
	err := row.Scan(&h.ID, &h.Name, &h.Phone, &h.Kind, &h.Status, &h.StatusReason, &h.IDLast4, &h.CreatedAt, &h.HasPhoto, &h.Rating, &h.RatingCount)
	if h.Rating != nil {
		r := float64(int(*h.Rating*10+0.5)) / 10
		h.Rating = &r
	}
	h.Flats = []Flat{}
	return &h, err
}

// fill adds flats (all of them for the office, only mine for a resident), my rating and today's status.
func (s *Service) fill(ctx context.Context, soc uuid.UUID, mine *uuid.UUID, list []*Helper) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	by := map[uuid.UUID]*Helper{}
	for i, h := range list {
		ids[i] = h.ID
		by[h.ID] = h
	}
	rows, err := s.pool.Query(ctx, `SELECT hf.helper_id, hf.unit_id, b.code || '-' || u.number, hf.days, hf.window_from::text, hf.window_to::text, hf.notify_entry, hf.notify_exit
		FROM helper_flats hf JOIN units u ON u.id=hf.unit_id JOIN blocks b ON b.id=u.block_id
		WHERE hf.helper_id = ANY($1) AND ($2::uuid IS NULL OR hf.unit_id=$2) ORDER BY b.code, u.number`, ids, mine)
	if err != nil {
		return err
	}
	for rows.Next() {
		var hid uuid.UUID
		var f Flat
		var days []int16
		if err := rows.Scan(&hid, &f.UnitID, &f.Flat, &days, &f.WindowFrom, &f.WindowTo, &f.NotifyEntry, &f.NotifyExit); err != nil {
			rows.Close()
			return err
		}
		f.Days = []int{}
		for _, d := range days {
			f.Days = append(f.Days, int(d))
		}
		trim := func(v *string) *string {
			if v != nil && len(*v) >= 5 {
				t := (*v)[:5]
				return &t
			}
			return v
		}
		f.WindowFrom, f.WindowTo = trim(f.WindowFrom), trim(f.WindowTo)
		by[hid].Flats = append(by[hid].Flats, f)
	}
	rows.Close()
	if mine != nil {
		r2, err := s.pool.Query(ctx, `SELECT helper_id, rating, comment FROM helper_ratings WHERE unit_id=$1 AND helper_id = ANY($2)`, *mine, ids)
		if err != nil {
			return err
		}
		for r2.Next() {
			var hid uuid.UUID
			var rt int
			var c *string
			if r2.Scan(&hid, &rt, &c) == nil {
				by[hid].MyRating, by[hid].MyComment = &rt, c
			}
		}
		r2.Close()
	}
	r3, err := s.pool.Query(ctx, `SELECT DISTINCT ON (a.subject_id) a.subject_id, a.in_at, a.out_at
		FROM attendance_logs a JOIN societies so ON so.id=a.society_id
		WHERE a.society_id=$1 AND a.subject_type='helper' AND a.subject_id = ANY($2)
		  AND (a.out_at IS NULL OR a.in_at >= date_trunc('day', now() AT TIME ZONE so.timezone) AT TIME ZONE so.timezone)
		ORDER BY a.subject_id, a.in_at DESC`, soc, ids)
	if err != nil {
		return err
	}
	defer r3.Close()
	for r3.Next() {
		var hid uuid.UUID
		var in time.Time
		var out *time.Time
		if r3.Scan(&hid, &in, &out) == nil {
			by[hid].Today = &Today{Inside: out == nil, InAt: &in, OutAt: out}
		}
	}
	return r3.Err()
}

// Mine lists the helpers of my flat.
func (s *Service) Mine(ctx context.Context, p authctx.Principal) ([]*Helper, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return []*Helper{}, nil
	}
	rows, err := s.pool.Query(ctx, helperSelect+`JOIN helper_flats hf ON hf.helper_id=h.id WHERE h.society_id=$1 AND hf.unit_id=$2 ORDER BY h.full_name, h.id`, p.SocietyID, *unit)
	if err != nil {
		return nil, err
	}
	out := []*Helper{}
	for rows.Next() {
		h, err := scanHelper(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	rows.Close()
	return out, s.fill(ctx, p.SocietyID, unit, out)
}

// linked returns the unit of the caller when the helper works for it.
func (s *Service) linked(ctx context.Context, p authctx.Principal, id uuid.UUID) (uuid.UUID, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return uuid.Nil, ErrNotFound
	}
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM helper_flats hf JOIN helpers h ON h.id=hf.helper_id WHERE hf.helper_id=$1 AND hf.unit_id=$2 AND h.society_id=$3`, id, *unit, p.SocietyID).Scan(&one)
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return *unit, nil
}

func (s *Service) MineByID(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Helper, error) {
	unit, err := s.linked(ctx, p, id)
	if err != nil {
		return nil, err
	}
	h, err := scanHelper(s.pool.QueryRow(ctx, helperSelect+`WHERE h.id=$1 AND h.society_id=$2`, id, p.SocietyID))
	if err != nil {
		return nil, ErrNotFound
	}
	list := []*Helper{h}
	return h, s.fill(ctx, p.SocietyID, &unit, list)
}

// Schedule is when a flat expects the helper.
type Schedule struct {
	Days        []int
	WindowFrom  string
	WindowTo    string
	NotifyEntry *bool
	NotifyExit  *bool
}

func (sc *Schedule) validate() error {
	seen := map[int]bool{}
	for _, d := range sc.Days {
		if d < 0 || d > 6 || seen[d] {
			return invalid("days must be unique numbers 0 (Sunday) to 6")
		}
		seen[d] = true
	}
	if (sc.WindowFrom == "") != (sc.WindowTo == "") {
		return invalid("give both a start and an end time, or neither")
	}
	if sc.WindowFrom != "" {
		a, ok1 := clock(sc.WindowFrom)
		b, ok2 := clock(sc.WindowTo)
		if !ok1 || !ok2 || a >= b {
			return invalid("times must be HH:MM with the start before the end")
		}
	}
	return nil
}

func clock(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

type RegisterInput struct {
	Name, Phone, Kind, IDLast4 string
	Schedule
}

// Register adds a helper to my flat. A person already known to the society (same phone) is linked, not duplicated.
func (s *Service) Register(ctx context.Context, p authctx.Principal, in RegisterInput) (*Helper, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return nil, ErrNoUnit
	}
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if l := len([]rune(in.Name)); l < 2 || l > 80 {
		return nil, invalid("name must be 2-80 characters")
	}
	in.Phone = resident.NormalizePhone(in.Phone)
	if !resident.ValidPhone(in.Phone) {
		return nil, httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	}
	if !kinds[in.Kind] {
		return nil, invalid("kind must be maid, cook, driver, nanny or other")
	}
	if in.IDLast4 != "" && !idRe.MatchString(in.IDLast4) {
		return nil, invalid("id_last4 must be 4 letters or digits")
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM helper_flats WHERE unit_id=$1`, *unit).Scan(&n); err != nil {
		return nil, err
	}
	if n >= maxPerFlat {
		return nil, ErrFlatLimit
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var hid uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `SELECT id, status FROM helpers WHERE society_id=$1 AND phone=$2 FOR UPDATE`, p.SocietyID, in.Phone).Scan(&hid, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		for try := 0; try < 8; try++ {
			code, cerr := pass.NewCode()
			if cerr != nil {
				return nil, cerr
			}
			sp, berr := tx.Begin(ctx) // savepoint: a code collision must not poison the transaction
			if berr != nil {
				return nil, berr
			}
			err = sp.QueryRow(ctx, `INSERT INTO helpers (society_id, full_name, phone, kind, id_last4, pass_code, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
				p.SocietyID, in.Name, in.Phone, in.Kind, nullable(in.IDLast4), code, p.MembershipID).Scan(&hid)
			if err == nil {
				err = sp.Commit(ctx)
				break
			}
			_ = sp.Rollback(ctx)
			if !database.IsUnique(err, "helpers_society_id_pass_code_key") {
				break
			}
		}
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case status == "blacklisted":
		return nil, ErrBlacklisted
	}
	_, err = tx.Exec(ctx, `INSERT INTO helper_flats (helper_id, unit_id, added_by, days, window_from, window_to, notify_entry, notify_exit)
		VALUES ($1,$2,$3,$4::smallint[],$5::time,$6::time,COALESCE($7,true),COALESCE($8,false))`,
		hid, *unit, p.MembershipID, toInt16(in.Days), nullable(in.WindowFrom), nullable(in.WindowTo), in.NotifyEntry, in.NotifyExit)
	if database.IsUnique(err, "") {
		return nil, ErrAlreadyLinked
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "helper.register", hid, map[string]any{"kind": in.Kind})
	return s.MineByID(ctx, p, hid)
}

func toInt16(d []int) []int16 {
	out := make([]int16, len(d))
	for i, v := range d {
		out[i] = int16(v)
	}
	return out
}

func (s *Service) UpdateLink(ctx context.Context, p authctx.Principal, id uuid.UUID, sc Schedule) (*Helper, error) {
	unit, err := s.linked(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if err := sc.validate(); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE helper_flats SET days=$3::smallint[], window_from=$4::time, window_to=$5::time, notify_entry=COALESCE($6,notify_entry), notify_exit=COALESCE($7,notify_exit)
		WHERE helper_id=$1 AND unit_id=$2`, id, unit, toInt16(sc.Days), nullable(sc.WindowFrom), nullable(sc.WindowTo), sc.NotifyEntry, sc.NotifyExit); err != nil {
		return nil, err
	}
	s.log(ctx, p, "helper.schedule", id, nil)
	return s.MineByID(ctx, p, id)
}

// Unlink removes the helper from my flat only; the person and their other flats stay.
func (s *Service) Unlink(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	unit, err := s.linked(ctx, p, id)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM helper_flats WHERE helper_id=$1 AND unit_id=$2`, id, unit); err != nil {
		return err
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM helper_ratings WHERE helper_id=$1 AND unit_id=$2`, id, unit)
	s.log(ctx, p, "helper.unlink", id, nil)
	return nil
}

type Pass struct {
	QR   string `json:"qr"`
	Code string `json:"code"`
}

func (s *Service) Pass(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Pass, error) {
	if _, err := s.linked(ctx, p, id); err != nil {
		return nil, err
	}
	var code string
	if err := s.pool.QueryRow(ctx, `SELECT pass_code FROM helpers WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&code); err != nil {
		return nil, ErrNotFound
	}
	return &Pass{QR: s.qr.QR(id), Code: code}, nil
}

// RegenerateCode replaces the 6-digit code (the QR stays valid); use it when the code was shared by mistake.
func (s *Service) RegenerateCode(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Pass, error) {
	if _, err := s.linked(ctx, p, id); err != nil {
		return nil, err
	}
	for try := 0; try < 8; try++ {
		code, err := pass.NewCode()
		if err != nil {
			return nil, err
		}
		tag, err := s.pool.Exec(ctx, `UPDATE helpers SET pass_code=$3 WHERE id=$1 AND society_id=$2`, id, p.SocietyID, code)
		if database.IsUnique(err, "") {
			continue
		}
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, ErrNotFound
		}
		s.log(ctx, p, "helper.new_code", id, nil)
		return &Pass{QR: s.qr.QR(id), Code: code}, nil
	}
	return nil, errors.New("could not generate a unique code")
}

func (s *Service) Rate(ctx context.Context, p authctx.Principal, id uuid.UUID, rating int, comment string) (*Helper, error) {
	unit, err := s.linked(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if rating < 1 || rating > 5 {
		return nil, invalid("rating must be 1-5")
	}
	comment = strings.TrimSpace(comment)
	if len([]rune(comment)) > 300 {
		return nil, invalid("the comment is limited to 300 characters")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO helper_ratings (helper_id, unit_id, rating, comment) VALUES ($1,$2,$3,$4)
		ON CONFLICT (helper_id, unit_id) DO UPDATE SET rating=EXCLUDED.rating, comment=EXCLUDED.comment, updated_at=now()`, id, unit, rating, nullable(comment)); err != nil {
		return nil, err
	}
	return s.MineByID(ctx, p, id)
}

// SetPhoto stores the helper's photo (replacing the old one). Linked residents may add it while the helper is not verified yet.
func (s *Service) SetPhoto(ctx context.Context, p authctx.Principal, id uuid.UUID, office bool, name string, data []byte) error {
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM helpers WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&status); err != nil {
		return ErrNotFound
	}
	if !office {
		if _, err := s.linked(ctx, p, id); err != nil {
			return err
		}
		if status == "verified" {
			return ErrInvalidState
		}
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM attachments WHERE owner_type='helper' AND owner_id=$1 AND society_id=$2`, id, p.SocietyID); err != nil {
		return err
	}
	if _, err := s.files.Save(ctx, p.SocietyID, attachment.OwnerHelper, id, p.UserID, name, data, 1); err != nil {
		return err
	}
	s.log(ctx, p, "helper.photo", id, nil)
	return nil
}

// Photo opens the helper's photo; residents only for their own helpers, the office and the gate for any.
func (s *Service) Photo(ctx context.Context, p authctx.Principal, id uuid.UUID, office bool) ([]byte, *attachment.File, error) {
	if !office {
		if _, err := s.linked(ctx, p, id); err != nil {
			return nil, nil, err
		}
	} else {
		var one int
		if err := s.pool.QueryRow(ctx, `SELECT 1 FROM helpers WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&one); err != nil {
			return nil, nil, ErrNotFound
		}
	}
	fs, err := s.files.List(ctx, p.SocietyID, attachment.OwnerHelper, id)
	if err != nil {
		return nil, nil, err
	}
	if len(fs) == 0 {
		return nil, nil, attachment.ErrNotFound
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerHelper, id, fs[0].ID)
}

// ── office ───────────────────────────────────────────────────────────────────

type Filter struct {
	Status, Kind, Query string
	Limit, Offset       int
}

func (s *Service) AdminList(ctx context.Context, p authctx.Principal, f Filter) ([]*Helper, int, error) {
	args := []any{p.SocietyID}
	where := `h.society_id=$1`
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "?", "$"+itoa(len(args)))
	}
	if f.Status != "" {
		add("h.status = ?", f.Status)
	}
	if f.Kind != "" {
		add("h.kind = ?", f.Kind)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		add("(h.full_name ILIKE ? OR h.phone LIKE ?)", "%"+q+"%")
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM helpers h WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, helperSelect+`WHERE `+where+` ORDER BY (h.status='pending') DESC, h.full_name, h.id LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2), append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	out := []*Helper{}
	for rows.Next() {
		h, err := scanHelper(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, h)
	}
	rows.Close()
	return out, total, s.fill(ctx, p.SocietyID, nil, out)
}

func itoa(n int) string {
	const d = "0123456789"
	if n < 10 {
		return d[n : n+1]
	}
	return itoa(n/10) + d[n%10:n%10+1]
}

func (s *Service) AdminGet(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Helper, error) {
	h, err := scanHelper(s.pool.QueryRow(ctx, helperSelect+`WHERE h.id=$1 AND h.society_id=$2`, id, p.SocietyID))
	if err != nil {
		return nil, ErrNotFound
	}
	return h, s.fill(ctx, p.SocietyID, nil, []*Helper{h})
}

var transitions = map[string]struct {
	from   []string
	to     string
	reason bool
	title  string
}{
	"verify":    {[]string{"pending", "suspended"}, "verified", false, "Verified"},
	"suspend":   {[]string{"pending", "verified"}, "suspended", true, "Suspended"},
	"blacklist": {[]string{"pending", "verified", "suspended"}, "blacklisted", true, "Blacklisted"},
	"restore":   {[]string{"suspended", "blacklisted"}, "pending", false, "Restored"},
}

// Decide moves a helper between verification states. Blacklisting also clears all flat links' alerts at the gate (the gate refuses the pass).
func (s *Service) Decide(ctx context.Context, p authctx.Principal, id uuid.UUID, action, reason string) (*Helper, error) {
	t, ok := transitions[action]
	if !ok {
		return nil, invalid("unknown action")
	}
	reason = strings.TrimSpace(reason)
	if t.reason && (len([]rune(reason)) < 3 || len([]rune(reason)) > 200) {
		return nil, invalid("give a reason of 3-200 characters")
	}
	var cur string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM helpers WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&cur); err != nil {
		return nil, ErrNotFound
	}
	allowed := false
	for _, f := range t.from {
		allowed = allowed || f == cur
	}
	if !allowed {
		return nil, ErrInvalidState
	}
	var verifiedBy *uuid.UUID
	if action == "verify" {
		verifiedBy = &p.MembershipID
	}
	var why *string
	if t.reason {
		why = &reason
	}
	if _, err := s.pool.Exec(ctx, `UPDATE helpers SET status=$3, status_reason=$4, verified_by=COALESCE($5, CASE WHEN $3='pending' THEN NULL ELSE verified_by END) WHERE id=$1 AND society_id=$2`,
		id, p.SocietyID, t.to, why, verifiedBy); err != nil {
		return nil, err
	}
	s.log(ctx, p, "helper."+action, id, map[string]any{"reason": reason})
	h, err := s.AdminGet(ctx, p, id)
	if err != nil {
		return nil, err
	}
	s.tellFlats(ctx, p.SocietyID, h, "helper.status", "Daily help "+strings.ToLower(t.title), h.Name+" is now "+t.to)
	return h, nil
}

func (s *Service) tellFlats(ctx context.Context, soc uuid.UUID, h *Helper, typ, title, body string) {
	if s.note == nil {
		return
	}
	var users []uuid.UUID
	for _, f := range h.Flats {
		users = append(users, s.note.UnitMembers(ctx, soc, f.UnitID)...)
	}
	if len(users) == 0 {
		return
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: typ, Title: title, Body: body,
		Data: map[string]any{"type": "helper", "helper_id": h.ID}, Priority: notify.PriorityNormal})
}
