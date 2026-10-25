// Package sos handles resident emergency alerts: raise, acknowledge, resolve, cancel and escalation.
package sos

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

const (
	StatusRaised       = "raised"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
	StatusCancelled    = "cancelled"

	maxEscalations = 3
	rateLimitPerHr = 3
	maxExport      = 10000
)

var (
	ErrNoUnit        = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "alert not found")
	ErrRateLimit     = httpx.NewError(http.StatusTooManyRequests, "SOS_RATE_LIMIT", "too many alerts in the last hour; call the security desk directly")
	ErrBadKind       = httpx.NewError(http.StatusBadRequest, "VALIDATION", "kind must be medical, fire, security or other")
	ErrCancelWindow  = httpx.NewError(http.StatusConflict, "CANCEL_WINDOW_OVER", "this alert can only be cancelled within the first 2 minutes")
	ErrNotYours      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "alert not found")
	ErrAdminOnly     = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "only the society office can read the alert log")
	ErrAlreadyClosed = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this alert is already closed")
)

var kinds = map[string]string{"medical": "Medical", "fire": "Fire", "security": "Security", "other": "Emergency"}

type Service struct {
	pool          *pgxpool.Pool
	audit         *audit.Logger
	note          *notify.Service
	hub           *realtime.Hub
	escalateAfter time.Duration
	cancelWindow  time.Duration
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, note *notify.Service, hub *realtime.Hub, escalateAfter, cancelWindow time.Duration) *Service {
	if escalateAfter <= 0 {
		escalateAfter = 60 * time.Second
	}
	if cancelWindow <= 0 {
		cancelWindow = 2 * time.Minute
	}
	return &Service{pool: pool, audit: a, note: note, hub: hub, escalateAfter: escalateAfter, cancelWindow: cancelWindow}
}

// Alert is one SOS. CallbackPhone is set only for responders while the alert is open.
type Alert struct {
	ID             uuid.UUID  `json:"id"`
	UnitID         uuid.UUID  `json:"unit_id"`
	Flat           string     `json:"flat"`
	Kind           string     `json:"kind"`
	Note           *string    `json:"note,omitempty"`
	IsTest         bool       `json:"is_test"`
	Status         string     `json:"status"`
	RaisedAt       time.Time  `json:"raised_at"`
	RaisedBy       string     `json:"raised_by"`
	CallbackPhone  *string    `json:"callback_phone,omitempty"`
	Escalations    int        `json:"escalations"`
	AcknowledgedBy *string    `json:"acknowledged_by,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedBy     *string    `json:"resolved_by,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolutionNote *string    `json:"resolution_note,omitempty"`
	CancelledAt    *time.Time `json:"cancelled_at,omitempty"`
	Merged         bool       `json:"merged,omitempty"`
	CanCancelUntil *time.Time `json:"can_cancel_until,omitempty"`
}

const selectAlert = `SELECT e.id, e.unit_id, b.code || '-' || un.number, e.kind, e.note, e.is_test, e.status, e.raised_at,
		ru.full_name, ru.phone, e.escalations, au.full_name, e.acknowledged_at, xu.full_name, e.resolved_at, e.resolution_note, e.cancelled_at
	FROM sos_events e JOIN units un ON un.id=e.unit_id JOIN blocks b ON b.id=un.block_id
	JOIN memberships rm ON rm.id=e.raised_by_membership_id JOIN users ru ON ru.id=rm.user_id
	LEFT JOIN memberships am ON am.id=e.acknowledged_by LEFT JOIN users au ON au.id=am.user_id
	LEFT JOIN memberships xm ON xm.id=e.resolved_by LEFT JOIN users xu ON xu.id=xm.user_id `

func (s *Service) scan(row pgx.Row, withPhone bool, extra ...any) (*Alert, error) {
	var a Alert
	var phone string
	dest := []any{&a.ID, &a.UnitID, &a.Flat, &a.Kind, &a.Note, &a.IsTest, &a.Status, &a.RaisedAt, &a.RaisedBy, &phone, &a.Escalations,
		&a.AcknowledgedBy, &a.AcknowledgedAt, &a.ResolvedBy, &a.ResolvedAt, &a.ResolutionNote, &a.CancelledAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if withPhone && (a.Status == StatusRaised || a.Status == StatusAcknowledged) {
		a.CallbackPhone = &phone
	}
	if a.Status == StatusRaised || a.Status == StatusAcknowledged {
		t := a.RaisedAt.Add(s.cancelWindow)
		a.CanCancelUntil = &t
	}
	return &a, nil
}

func (s *Service) one(ctx context.Context, soc, id uuid.UUID, withPhone bool) (*Alert, error) {
	return s.scan(s.pool.QueryRow(ctx, selectAlert+`WHERE e.id=$1 AND e.society_id=$2`, id, soc), withPhone)
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "sos", EntityID: id.String(), Meta: meta})
}

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) (uuid.UUID, error) {
	var u *uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	if err != nil || u == nil {
		return uuid.Nil, ErrNoUnit
	}
	return *u, nil
}

var officeRoles = []string{"society_admin", "committee"}

func isOffice(role string) bool { return role == "society_admin" || role == "committee" }

// ── raise ────────────────────────────────────────────────────────────────────

// Raise records an SOS and alerts every guard and the society office at once. A real alert
// while one is already open for the flat joins it; a test alert only reaches the raiser and the office.
func (s *Service) Raise(ctx context.Context, p authctx.Principal, kind string, note *string, test bool) (*Alert, error) {
	if _, ok := kinds[kind]; !ok {
		return nil, ErrBadKind
	}
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	if note != nil {
		n := strings.TrimSpace(*note)
		if n == "" {
			note = nil
		} else {
			if len([]rune(n)) > 200 {
				n = string([]rune(n)[:200])
			}
			note = &n
		}
	}
	if !test { // join an alert that is already open for this flat
		var open uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT id FROM sos_events WHERE society_id=$1 AND unit_id=$2 AND NOT is_test AND status IN ('raised','acknowledged') ORDER BY raised_at DESC LIMIT 1`, p.SocietyID, unit).Scan(&open)
		if err == nil {
			a, err := s.one(ctx, p.SocietyID, open, false)
			if err == nil {
				a.Merged = true
			}
			return a, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var recent int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sos_events WHERE raised_by_membership_id=$1 AND raised_at > now() - interval '1 hour'`, p.MembershipID).Scan(&recent); err != nil {
		return nil, err
	}
	if recent >= rateLimitPerHr {
		return nil, ErrRateLimit
	}
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, `INSERT INTO sos_events (society_id, unit_id, raised_by_membership_id, kind, note, is_test) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		p.SocietyID, unit, p.MembershipID, kind, note, test).Scan(&id); err != nil {
		return nil, err
	}
	s.log(ctx, p, "sos.raise", id, map[string]any{"kind": kind, "test": test})
	a, err := s.one(ctx, p.SocietyID, id, false)
	if err != nil {
		return nil, err
	}
	s.fanOut(ctx, p.SocietyID, p.UserID, a, "sos.raised", "")
	return a, nil
}

// payload is what live screens need; the callback phone is only ever fetched over REST by responders.
func payload(a *Alert) map[string]any {
	return map[string]any{
		"sos_id": a.ID, "kind": a.Kind, "flat": a.Flat, "status": a.Status, "is_test": a.IsTest, "note": a.Note, "raised_at": a.RaisedAt,
		"escalations": a.Escalations, "acknowledged_by": a.AcknowledgedBy, "resolved_by": a.ResolvedBy,
	}
}

// fanOut sends the realtime event and the notifications for one transition.
func (s *Service) fanOut(ctx context.Context, soc, raiser uuid.UUID, a *Alert, typ, headline string) {
	ev := realtime.Event{Type: typ, SocietyID: soc, Unit: &a.UnitID, Data: payload(a)}
	if a.IsTest {
		ev.Unit = nil
		ev.User = &raiser
		ev.Roles = officeRoles
	} else {
		ev.Guards = true
		ev.Roles = officeRoles
	}
	if s.hub != nil {
		s.hub.Publish(ctx, ev)
		if a.IsTest { // the raiser's other family members need not hear a test
			return
		}
	}
	if s.note == nil {
		return
	}
	kind := kinds[a.Kind]
	responders := append(s.note.ByRoles(ctx, soc, "guard", "society_admin", "committee"), []uuid.UUID{}...)
	family := s.note.UnitMembers(ctx, soc, a.UnitID)
	if a.IsTest {
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: append([]uuid.UUID{raiser}, s.note.ByRoles(ctx, soc, officeRoles...)...), Type: typ,
			Title: "Test emergency alert", Body: "This is a test from flat " + a.Flat + ". No guard was alarmed.",
			Data: map[string]any{"type": "sos", "sos_id": a.ID, "test": true}, Priority: notify.PriorityNormal})
		return
	}
	switch typ {
	case "sos.raised", "sos.escalated":
		title := "Emergency alert"
		if typ == "sos.escalated" {
			title = "Emergency alert – still unanswered"
		}
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: responders, Type: typ, Title: title, Body: kind + " emergency at flat " + a.Flat,
			Data: map[string]any{"type": "sos", "sos_id": a.ID, "role": "responder"}, Priority: notify.PriorityCritical})
		if typ == "sos.raised" {
			s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: family, Type: typ, Title: "SOS sent", Body: "Your emergency alert was sent to security.",
				Data: map[string]any{"type": "sos", "sos_id": a.ID, "role": "resident"}, Priority: notify.PriorityCritical})
		}
	case "sos.acknowledged":
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: family, Type: typ, Title: "Help is on the way", Body: headline,
			Data: map[string]any{"type": "sos", "sos_id": a.ID, "role": "resident"}, Priority: notify.PriorityCritical})
	case "sos.resolved":
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: family, Type: typ, Title: "Emergency resolved", Body: "Security closed your alert.",
			Data: map[string]any{"type": "sos", "sos_id": a.ID, "role": "resident"}, Priority: notify.PriorityNormal})
	case "sos.cancelled":
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: responders, Type: typ, Title: "Alert cancelled", Body: "Flat " + a.Flat + " cancelled the alert (false alarm).",
			Data: map[string]any{"type": "sos", "sos_id": a.ID, "role": "responder"}, Priority: notify.PriorityNormal})
	}
}

// ── cancel ───────────────────────────────────────────────────────────────────

func (s *Service) Cancel(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Alert, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	cur, err := s.one(ctx, p.SocietyID, id, false)
	if err != nil {
		return nil, err
	}
	var mine bool
	if err := s.pool.QueryRow(ctx, `SELECT raised_by_membership_id=$3 AND unit_id=$4 FROM sos_events WHERE id=$1 AND society_id=$2`, id, p.SocietyID, p.MembershipID, unit).Scan(&mine); err != nil || !mine {
		return nil, ErrNotYours
	}
	if cur.Status != StatusRaised && cur.Status != StatusAcknowledged {
		return nil, ErrAlreadyClosed
	}
	if time.Since(cur.RaisedAt) > s.cancelWindow {
		return nil, ErrCancelWindow
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sos_events SET status='cancelled', cancelled_at=now() WHERE id=$1 AND society_id=$2 AND status IN ('raised','acknowledged')`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrAlreadyClosed
	}
	s.log(ctx, p, "sos.cancel", id, nil)
	a, err := s.one(ctx, p.SocietyID, id, false)
	if err == nil {
		s.fanOut(ctx, p.SocietyID, p.UserID, a, "sos.cancelled", "")
	}
	return a, err
}

// ── responders ───────────────────────────────────────────────────────────────

// Active lists the open alerts with the callback number. Guards never see test alerts.
func (s *Service) Active(ctx context.Context, p authctx.Principal) ([]Alert, error) {
	q := selectAlert + `WHERE e.society_id=$1 AND e.status IN ('raised','acknowledged')`
	if !isOffice(p.Role) {
		q += ` AND NOT e.is_test`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY e.raised_at DESC LIMIT 50`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := s.scan(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// visibleTo hides test alerts from guards (404, as if they never existed).
func (s *Service) visibleTo(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Alert, error) {
	a, err := s.one(ctx, p.SocietyID, id, true)
	if err != nil {
		return nil, err
	}
	if a.IsTest && !isOffice(p.Role) {
		return nil, ErrNotFound
	}
	return a, nil
}

func (s *Service) Acknowledge(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Alert, error) {
	cur, err := s.visibleTo(ctx, p, id)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sos_events SET status='acknowledged', acknowledged_by=$3, acknowledged_at=now() WHERE id=$1 AND society_id=$2 AND status='raised'`, id, p.SocietyID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		if cur.Status == StatusAcknowledged && cur.AcknowledgedBy != nil {
			return nil, httpx.NewError(http.StatusConflict, "ALREADY_ACKNOWLEDGED", *cur.AcknowledgedBy+" is already responding")
		}
		return nil, ErrAlreadyClosed
	}
	s.log(ctx, p, "sos.acknowledge", id, nil)
	a, err := s.one(ctx, p.SocietyID, id, true)
	if err == nil {
		who := ""
		if a.AcknowledgedBy != nil {
			who = *a.AcknowledgedBy + " is on the way."
		}
		s.fanOut(ctx, p.SocietyID, uuid.Nil, a, "sos.acknowledged", who)
	}
	return a, err
}

func (s *Service) Resolve(ctx context.Context, p authctx.Principal, id uuid.UUID, outcome string) (*Alert, error) {
	if _, err := s.visibleTo(ctx, p, id); err != nil {
		return nil, err
	}
	outcome = strings.TrimSpace(outcome)
	if len([]rune(outcome)) > 300 {
		outcome = string([]rune(outcome)[:300])
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sos_events SET status='resolved', resolved_by=$3, resolved_at=now(), resolution_note=NULLIF($4,'') WHERE id=$1 AND society_id=$2 AND status IN ('raised','acknowledged')`,
		id, p.SocietyID, p.MembershipID, outcome)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrAlreadyClosed
	}
	s.log(ctx, p, "sos.resolve", id, nil)
	a, err := s.one(ctx, p.SocietyID, id, false)
	if err == nil {
		s.fanOut(ctx, p.SocietyID, uuid.Nil, a, "sos.resolved", "")
	}
	return a, err
}

// ── escalation ───────────────────────────────────────────────────────────────

// Sweep re-alerts real alerts nobody has acknowledged (every escalateAfter, at most 3 times).
func (s *Service) Sweep(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`UPDATE sos_events SET escalations=escalations+1, last_alert_at=now()
		WHERE status='raised' AND NOT is_test AND escalations < %d AND last_alert_at < now() - $1::interval RETURNING id, society_id`, maxEscalations),
		fmt.Sprintf("%d seconds", int(s.escalateAfter.Seconds())))
	if err != nil {
		return 0, err
	}
	type hit struct{ id, soc uuid.UUID }
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.id, &h.soc) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	for _, h := range hits {
		if a, err := s.one(ctx, h.soc, h.id, false); err == nil {
			s.fanOut(ctx, h.soc, uuid.Nil, a, "sos.escalated", "")
		}
	}
	return len(hits), rows.Err()
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("sos")
			_, _ = s.Sweep(ctx)
		}
	}
}

// ── history ──────────────────────────────────────────────────────────────────

// Mine is my own alert history.
func (s *Service) Mine(ctx context.Context, p authctx.Principal, limit, offset int) ([]Alert, int, error) {
	rows, err := s.pool.Query(ctx, selectAlert+`WHERE e.raised_by_membership_id=$1 ORDER BY e.raised_at DESC LIMIT $2 OFFSET $3`, p.MembershipID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := s.scan(rows, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *a)
	}
	var total int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM sos_events WHERE raised_by_membership_id=$1`, p.MembershipID).Scan(&total)
	return out, total, rows.Err()
}

type Filter struct {
	Status string
	Kind   string
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

func (s *Service) adminWhere(soc uuid.UUID, f Filter) (string, []any) {
	args := []any{soc}
	where := `e.society_id=$1`
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args)))
	}
	if f.Status != "" {
		add("e.status = ?", f.Status)
	}
	if f.Kind != "" {
		add("e.kind = ?", f.Kind)
	}
	if f.From != nil {
		add("e.raised_at >= ?", *f.From)
	}
	if f.To != nil {
		add("e.raised_at < ?", *f.To)
	}
	return where, args
}

// AdminList is the society's alert log (office roles only).
func (s *Service) AdminList(ctx context.Context, p authctx.Principal, f Filter) ([]Alert, int, error) {
	if !isOffice(p.Role) {
		return nil, 0, ErrAdminOnly
	}
	where, args := s.adminWhere(p.SocietyID, f)
	rows, err := s.pool.Query(ctx, selectAlert+`WHERE `+where+fmt.Sprintf(` ORDER BY e.raised_at DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := s.scan(rows, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *a)
	}
	var total int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM sos_events e WHERE `+where, args...).Scan(&total)
	return out, total, rows.Err()
}

func (s *Service) AdminGet(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Alert, error) {
	if !isOffice(p.Role) {
		return nil, ErrAdminOnly
	}
	return s.one(ctx, p.SocietyID, id, true)
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func ts(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// AdminExport writes the filtered log as CSV (formula-safe, capped).
func (s *Service) AdminExport(ctx context.Context, p authctx.Principal, f Filter, w io.Writer) (int, error) {
	f.Limit, f.Offset = maxExport, 0
	rows, _, err := s.AdminList(ctx, p, f)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Raised", "Kind", "Flat", "Raised by", "Status", "Test", "Escalations", "Acknowledged by", "Acknowledged at", "Resolved by", "Resolved at", "Note", "Outcome"})
	for _, a := range rows {
		_ = cw.Write([]string{ts(&a.RaisedAt), a.Kind, a.Flat, csvSafe(a.RaisedBy), a.Status, fmt.Sprint(a.IsTest), fmt.Sprint(a.Escalations),
			csvSafe(str(a.AcknowledgedBy)), ts(a.AcknowledgedAt), csvSafe(str(a.ResolvedBy)), ts(a.ResolvedAt), csvSafe(str(a.Note)), csvSafe(str(a.ResolutionNote))})
	}
	cw.Flush()
	s.log(ctx, p, "sos.export", uuid.Nil, map[string]any{"rows": len(rows)})
	return len(rows), cw.Error()
}
