package visitor

import (
	"context"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/household"
	"github.com/Nexora48/nexora-api/internal/pass"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

const (
	maxActiveInvites = 20
	defaultWindow    = 4 * time.Hour
	earlyGrace       = 10 * time.Minute
	walkInWindow     = 2 * time.Hour
	verifyMaxFails   = 10
	verifyLockWindow = 5 * time.Minute
)

var (
	ErrNoUnit           = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrNotFound         = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "visit not found")
	ErrInviteNotAllowed = httpx.NewError(http.StatusForbidden, "INVITE_NOT_ALLOWED", "you are not allowed to invite visitors")
	ErrInviteLimit      = httpx.NewError(http.StatusConflict, "INVITE_LIMIT", "this flat already has 20 open passes; cancel or wait for some to expire")
	ErrInvalidWindow    = httpx.NewError(http.StatusBadRequest, "INVALID_WINDOW", "the visit window is invalid")
	ErrInvalidPhone     = httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	ErrInvalidVehicle   = httpx.NewError(http.StatusBadRequest, "VALIDATION", "vehicle number must be 4-12 letters/digits")
	ErrVerifyLocked     = httpx.NewError(http.StatusTooManyRequests, "VERIFY_LOCKED", "too many wrong codes, try again in 5 minutes")
	ErrUnitNotFound     = httpx.NewError(http.StatusNotFound, "UNIT_NOT_FOUND", "flat not found in this society")
	ErrPhotoPending     = httpx.NewError(http.StatusConflict, "PHOTO_PENDING", "the guard is still taking the visitor's photo")
	ErrRingLimit        = httpx.NewError(http.StatusTooManyRequests, "RING_LIMIT", "you can ring again in a minute (max 3 times)")
	ErrNoBlobs          = httpx.NewError(http.StatusServiceUnavailable, "PHOTOS_UNAVAILABLE", "photo storage is not configured")
	ErrBadPhoto         = httpx.NewError(http.StatusBadRequest, "BAD_PHOTO", "send a JPEG or PNG image up to 2 MB")
	ErrNoPhoto          = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "no photo for this visit")
	ErrNoResident       = httpx.NewError(http.StatusConflict, "NO_RESIDENT", "nobody lives in this flat")
)

func invalidState(status string) *httpx.AppError {
	return httpx.NewError(http.StatusConflict, "INVALID_STATE", "this visit is "+status)
}

func passNotValid(reason string) *httpx.AppError {
	return httpx.NewError(http.StatusConflict, "PASS_NOT_VALID", "pass is not valid: "+reason)
}

var vehicleRe = regexp.MustCompile(`^[A-Z0-9]{4,12}$`)

type Service struct {
	pool       *pgxpool.Pool
	rdb        *redis.Client
	audit      *audit.Logger
	qr         *pass.Signer
	maxWindow  time.Duration
	walkinWait time.Duration
	ev         Events
	rules      RuleResolver
	blobs      blob.Store
}

// Events lets the process publish visit changes (realtime + push) without this package knowing how.
type Events interface {
	VisitEvent(ctx context.Context, typ string, v *Visit)
}

// SetEvents wires the realtime/push dispatcher; nil keeps visit changes silent.
func (s *Service) SetEvents(e Events) { s.ev = e }

// RuleResolver tells the gate how a flat wants deliveries and cabs handled.
type RuleResolver interface {
	Resolve(ctx context.Context, society, unit uuid.UUID, purpose string, company *string) (RuleDecision, error)
	Consume(ctx context.Context, expectationID uuid.UUID) error
}

// RuleDecision: Mode is "" (ask the resident), "auto_approve" or "leave_at_gate".
type RuleDecision struct {
	Mode          string
	ExpectationID *uuid.UUID
}

// SetRules enables per-flat delivery and cab rules.
func (s *Service) SetRules(r RuleResolver) { s.rules = r }

// SetBlobs enables walk-in photos.
func (s *Service) SetBlobs(b blob.Store) { s.blobs = b }

func (s *Service) emit(ctx context.Context, typ string, v *Visit) {
	if s.ev != nil && v != nil {
		s.ev.VisitEvent(ctx, typ, v)
	}
}

func NewService(pool *pgxpool.Pool, rdb *redis.Client, a *audit.Logger, secret string, maxWindow, walkinWait time.Duration) *Service {
	if maxWindow <= 0 {
		maxWindow = 7 * 24 * time.Hour
	}
	if walkinWait <= 0 {
		walkinWait = 10 * time.Minute
	}
	return &Service{pool: pool, rdb: rdb, audit: a, qr: pass.NewSigner(secret, "visitor-qr", qrPrefix), maxWindow: maxWindow, walkinWait: walkinWait}
}

// eff is the SQL for the effective status: stale invites/requests read as expired right away.
func (s *Service) eff() string {
	return fmt.Sprintf(`(CASE
		WHEN v.status='invited'  AND v.expected_until < now() THEN 'expired'
		WHEN v.status='approved' AND v.expected_until < now() THEN 'expired'
		WHEN v.status='waiting'  AND v.created_at < now() - interval '%d seconds' THEN 'expired'
		ELSE v.status END)`, int(s.walkinWait.Seconds()))
}

func (s *Service) selectCols() string {
	return `SELECT v.id, v.unit_id, un.number, b.code,
		COALESCE(hu.full_name, (SELECT us.full_name FROM memberships mm JOIN users us ON us.id=mm.user_id
		    WHERE mm.unit_id=v.unit_id AND mm.status IN ('active','suspended') AND mm.role_key IN ('resident_tenant','resident_owner')
		    ORDER BY (mm.role_key='resident_tenant') DESC LIMIT 1)),
		v.visitor_name, v.visitor_phone, v.purpose, v.company, v.vehicle_number, v.visitor_count, v.note, v.source,
		` + s.eff() + `, v.pass_code, v.expected_from, v.expected_until, v.entered_at, v.exited_at, v.decided_at, v.decision_reason, v.created_at,
		g.name, (v.photo_key IS NOT NULL), v.awaiting_photo, v.rerings::int, v.decided_by_rule, v.handover`
}

const fromJoin = ` FROM visits v JOIN units un ON un.id=v.unit_id JOIN blocks b ON b.id=un.block_id
	LEFT JOIN memberships hm ON hm.id=v.host_membership_id LEFT JOIN users hu ON hu.id=hm.user_id LEFT JOIN gates g ON g.id=v.gate_id `

func (s *Service) scan(row pgx.Row, extra ...any) (*Visit, error) {
	var v Visit
	dest := []any{&v.ID, &v.UnitID, &v.UnitNumber, &v.BlockCode, &v.HostName, &v.VisitorName, &v.VisitorPhone, &v.Purpose, &v.Company,
		&v.VehicleNumber, &v.VisitorCount, &v.Note, &v.Source, &v.Status, &v.PassCode, &v.ExpectedFrom, &v.ExpectedUntil, &v.EnteredAt,
		&v.ExitedAt, &v.DecidedAt, &v.DecisionReason, &v.CreatedAt, &v.GateName, &v.HasPhoto, &v.AwaitingPhoto, &v.Rerings, &v.DecidedByRule, &v.Handover}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.finish(&v)
	return &v, nil
}

// finish hides pass data once a visit is no longer an open invite and derives the QR payload.
func (s *Service) finish(v *Visit) {
	if v.Status != StatusInvited {
		v.PassCode = nil
		return
	}
	qr := s.qrPayload(v.ID)
	v.QR = &qr
}

func (s *Service) one(ctx context.Context, societyID, id uuid.UUID, unitID *uuid.UUID) (*Visit, error) {
	q := s.selectCols() + fromJoin + ` WHERE v.id=$1 AND v.society_id=$2`
	args := []any{id, societyID}
	if unitID != nil {
		q += ` AND v.unit_id=$3`
		args = append(args, *unitID)
	}
	return s.scan(s.pool.QueryRow(ctx, q, args...))
}

func (s *Service) many(ctx context.Context, where []string, args []any, order string, limit, offset int) ([]Visit, int, error) {
	q := s.selectCols() + `, count(*) OVER()` + fromJoin + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order +
		fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	rows, err := s.pool.Query(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Visit{}
	total := 0
	for rows.Next() {
		v, err := s.scanRow(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *v)
	}
	return out, total, rows.Err()
}

func (s *Service) scanRow(row pgx.Row, total *int) (*Visit, error) { return s.scan(row, total) }

// ── caller context ───────────────────────────────────────────────────────────

type member struct {
	UnitID    *uuid.UUID
	CanInvite bool
}

func (s *Service) member(ctx context.Context, p authctx.Principal) (*member, error) {
	var m member
	err := s.pool.QueryRow(ctx, `SELECT unit_id, can_invite_visitors FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`,
		p.MembershipID, p.UserID, p.SocietyID).Scan(&m.UnitID, &m.CanInvite)
	if err != nil {
		return nil, ErrNoUnit
	}
	return &m, nil
}

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) (uuid.UUID, *member, error) {
	m, err := s.member(ctx, p)
	if err != nil || m.UnitID == nil {
		return uuid.Nil, nil, ErrNoUnit
	}
	return *m.UnitID, m, nil
}

func (s *Service) logv(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "visit", EntityID: id.String(), Meta: meta})
}

// ── input checks ─────────────────────────────────────────────────────────────

func cleanPhone(p *string) (*string, error) {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil, nil
	}
	n := resident.NormalizePhone(*p)
	if !resident.ValidPhone(n) {
		return nil, ErrInvalidPhone
	}
	return &n, nil
}

func cleanVehicle(v *string) (*string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil, nil
	}
	n := household.NormalizeVehicle(*v)
	if !vehicleRe.MatchString(n) {
		return nil, ErrInvalidVehicle
	}
	return &n, nil
}

func cleanText(t *string) *string {
	if t == nil {
		return nil
	}
	v := strings.TrimSpace(*t)
	if v == "" {
		return nil
	}
	return &v
}

// ── resident: create / list / get / cancel / decide ──────────────────────────

func (s *Service) Create(ctx context.Context, p authctx.Principal, in CreateInput) (*Visit, error) {
	unitID, m, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	if !m.CanInvite {
		return nil, ErrInviteNotAllowed
	}
	phone, err := cleanPhone(in.VisitorPhone)
	if err != nil {
		return nil, err
	}
	vehicle, err := cleanVehicle(in.VehicleNumber)
	if err != nil {
		return nil, err
	}
	count := in.VisitorCount
	if count == 0 {
		count = 1
	}

	now := time.Now()
	from := now
	if in.ExpectedFrom != nil {
		from = *in.ExpectedFrom
	}
	until := from.Add(defaultWindow)
	if in.ExpectedUntil != nil {
		until = *in.ExpectedUntil
	}
	if from.Before(now.Add(-5*time.Minute)) || !until.After(from) || until.Before(now) || until.Sub(from) > s.maxWindow {
		return nil, ErrInvalidWindow
	}

	var open int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM visits WHERE society_id=$1 AND unit_id=$2 AND status='invited' AND expected_until >= now()`,
		p.SocietyID, unitID).Scan(&open); err != nil {
		return nil, err
	}
	if open >= maxActiveInvites {
		return nil, ErrInviteLimit
	}

	var id uuid.UUID
	for attempt := 0; ; attempt++ {
		code, err := newPassCode()
		if err != nil {
			return nil, err
		}
		err = s.pool.QueryRow(ctx, `
			INSERT INTO visits (society_id, unit_id, host_membership_id, created_by_membership_id, visitor_name, visitor_phone, purpose, company,
			                    vehicle_number, visitor_count, note, source, status, pass_code, expected_from, expected_until)
			VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,'pre_approved','invited',$11,$12,$13) RETURNING id`,
			p.SocietyID, unitID, p.MembershipID, strings.TrimSpace(in.VisitorName), phone, in.Purpose, cleanText(in.Company), vehicle, count,
			cleanText(in.Note), code, from, until).Scan(&id)
		if database.IsUnique(err, "visits_open_pass_code_uq") && attempt < 10 {
			continue // code collision among open passes: draw another
		}
		if err != nil {
			return nil, err
		}
		break
	}
	s.logv(ctx, p, "visit.create", id, map[string]any{"purpose": in.Purpose, "unit_id": unitID.String()})
	return s.one(ctx, p.SocietyID, id, &unitID)
}

func (s *Service) List(ctx context.Context, p authctx.Principal, f ListFilter) ([]Visit, int, error) {
	unitID, _, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	args := []any{p.SocietyID, unitID}
	where := []string{"v.society_id=$1", "v.unit_id=$2"}
	e := s.eff()
	order := "v.created_at DESC, v.id"
	switch f.Tab {
	case "active":
		where = append(where, e+" IN ('waiting','approved','inside')")
	case "history":
		where = append(where, e+" IN ('completed','rejected','expired','cancelled')")
	case "invited":
		where = append(where, e+" = 'invited'")
		order = "v.expected_from ASC, v.id"
	}
	if f.Purpose != "" {
		args = append(args, f.Purpose)
		where = append(where, fmt.Sprintf("v.purpose=$%d", len(args)))
	}
	return s.many(ctx, where, args, order, f.Limit, f.Offset)
}

func (s *Service) Get(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	unitID, _, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	return s.one(ctx, p.SocietyID, id, &unitID)
}

func (s *Service) Summary(ctx context.Context, p authctx.Principal) (*Summary, error) {
	unitID, _, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	e := s.eff()
	var sm Summary
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE %[1]s IN ('waiting','approved','inside')),
		       count(*) FILTER (WHERE %[1]s = 'invited'),
		       count(*) FILTER (WHERE v.created_at >= (date_trunc('day', now() AT TIME ZONE so.timezone) AT TIME ZONE so.timezone) AND v.status <> 'cancelled')
		FROM visits v JOIN societies so ON so.id=v.society_id WHERE v.society_id=$1 AND v.unit_id=$2`, e), p.SocietyID, unitID).
		Scan(&sm.Active, &sm.Invited, &sm.Today)
	return &sm, err
}

// classify explains why a transition did not apply (404 vs wrong state).
func (s *Service) classify(ctx context.Context, societyID, id uuid.UUID, unitID *uuid.UUID) error {
	v, err := s.one(ctx, societyID, id, unitID)
	if err != nil {
		return err
	}
	return invalidState(v.Status)
}

func (s *Service) Cancel(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	unitID, _, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE visits SET status='cancelled', updated_at=now()
		WHERE id=$1 AND society_id=$2 AND unit_id=$3 AND status='invited' AND expected_until >= now()`, id, p.SocietyID, unitID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, s.classify(ctx, p.SocietyID, id, &unitID)
	}
	s.logv(ctx, p, "visit.cancel", id, nil)
	v, err := s.one(ctx, p.SocietyID, id, &unitID)
	s.emit(ctx, "visit.cancelled", v)
	return v, err
}

// Decide lets a member of the flat approve or reject a waiting walk-in.
func (s *Service) Decide(ctx context.Context, p authctx.Principal, id uuid.UUID, approve bool, reason string) (*Visit, error) {
	unitID, _, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	var pending bool
	_ = s.pool.QueryRow(ctx, `SELECT awaiting_photo FROM visits WHERE id=$1 AND society_id=$2 AND unit_id=$3`, id, p.SocietyID, unitID).Scan(&pending)
	if pending {
		return nil, ErrPhotoPending
	}
	status := StatusRejected
	if approve {
		status = StatusApproved
	}
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE visits SET status=$4, decided_by_membership_id=$5, decided_at=now(), decision_reason=NULLIF($6,''), updated_at=now()
		WHERE id=$1 AND society_id=$2 AND unit_id=$3 AND status='waiting' AND created_at >= now() - interval '%d seconds'`,
		int(s.walkinWait.Seconds())), id, p.SocietyID, unitID, status, p.MembershipID, strings.TrimSpace(reason))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, s.classify(ctx, p.SocietyID, id, &unitID)
	}
	s.logv(ctx, p, "visit.decision", id, map[string]any{"decision": status})
	v, err := s.one(ctx, p.SocietyID, id, &unitID)
	s.emit(ctx, "visit.decided", v)
	return v, err
}

// ── sweeper ──────────────────────────────────────────────────────────────────

// Sweep persists the expiry that reads already show.
func (s *Service) Sweep(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		UPDATE visits SET status='expired', updated_at=now()
		WHERE (status IN ('invited','approved') AND expected_until < now())
		   OR (status='waiting' AND created_at < now() - interval '%d seconds')
		RETURNING id, society_id, source`, int(s.walkinWait.Seconds())))
	if err != nil {
		return 0, err
	}
	type hit struct{ id, soc uuid.UUID }
	var walkIns []hit
	var n int64
	for rows.Next() {
		var h hit
		var src string
		if rows.Scan(&h.id, &h.soc, &src) == nil {
			n++
			if src == "walk_in" {
				walkIns = append(walkIns, h)
			}
		}
	}
	rows.Close()
	for _, h := range walkIns { // only gate requests matter live
		if v, err := s.one(ctx, h.soc, h.id, nil); err == nil {
			s.emit(ctx, "visit.expired", v)
		}
	}
	return n, rows.Err()
}

// RunSweeper calls Sweep every interval until ctx is cancelled.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("visitors")
			_, _ = s.Sweep(ctx)
		}
	}
}
