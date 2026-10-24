// Package gate manages guards, gates and shifts, and replays actions queued offline on a guard device.
package gate

import (
	"context"
	"errors"
	"github.com/Nexora48/nexora-api/internal/attendance"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/resident"
)

var (
	ErrGuardNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "guard not found")
	ErrGateNotFound  = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "gate not found")
	ErrGuardExists   = httpx.NewError(http.StatusConflict, "GUARD_EXISTS", "this person already belongs to the society")
	ErrGateExists    = httpx.NewError(http.StatusConflict, "GATE_EXISTS", "a gate with this name already exists")
	ErrGateInactive  = httpx.NewError(http.StatusConflict, "GATE_INACTIVE", "this gate is switched off")
	ErrNoShift       = httpx.NewError(http.StatusConflict, "NO_SHIFT", "start a shift first")
	ErrBadName       = httpx.NewError(http.StatusBadRequest, "VALIDATION", "name must be 2-80 characters")
)

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	hub   *realtime.Hub
	att   *attendance.Service
	feats *feature.Service
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, hub *realtime.Hub) *Service {
	return &Service{pool: pool, audit: a, hub: hub}
}

// SetAttendance lets the offline batch and the pass sync carry daily-help and staff attendance.
func (s *Service) SetAttendance(att *attendance.Service, feats *feature.Service) {
	s.att, s.feats = att, feats
}

func (s *Service) attendanceOn(ctx context.Context, soc uuid.UUID) bool {
	if s.att == nil || s.feats == nil {
		return false
	}
	on, err := s.feats.Enabled(ctx, soc, "staff_attendance")
	return err == nil && on
}

// AttendanceSync is the offline list of helpers and staff (empty when the service is switched off for the society).
func (s *Service) AttendanceSync(ctx context.Context, p authctx.Principal) ([]attendance.SyncEntry, error) {
	if !s.attendanceOn(ctx, p.SocietyID) {
		return []attendance.SyncEntry{}, nil
	}
	return s.att.Sync(ctx, p)
}

func (s *Service) log(ctx context.Context, soc, actor uuid.UUID, action, entity string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &actor, SocietyID: &soc, Action: action, Entity: entity, EntityID: id.String(), Meta: meta})
}

// ── gates ────────────────────────────────────────────────────────────────────

type Gate struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Active bool      `json:"active"`
}

func (s *Service) Gates(ctx context.Context, soc uuid.UUID, onlyActive bool) ([]Gate, error) {
	// every society always has at least one gate
	if _, err := s.pool.Exec(ctx, `INSERT INTO gates (society_id, name) SELECT $1, 'Main Gate' WHERE NOT EXISTS (SELECT 1 FROM gates WHERE society_id=$1)`, soc); err != nil {
		return nil, err
	}
	q := `SELECT id, name, active FROM gates WHERE society_id=$1`
	if onlyActive {
		q += ` AND active`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY lower(name)`, soc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Gate{}
	for rows.Next() {
		var g Gate
		if err := rows.Scan(&g.ID, &g.Name, &g.Active); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func cleanName(n string, max int) (string, bool) {
	n = strings.TrimSpace(n)
	return n, len([]rune(n)) >= 2 && len([]rune(n)) <= max
}

func (s *Service) CreateGate(ctx context.Context, soc, actor uuid.UUID, name string) (*Gate, error) {
	name, ok := cleanName(name, 40)
	if !ok {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "gate name must be 2-40 characters")
	}
	var g Gate
	err := s.pool.QueryRow(ctx, `INSERT INTO gates (society_id, name) VALUES ($1,$2) RETURNING id, name, active`, soc, name).Scan(&g.ID, &g.Name, &g.Active)
	if database.IsUnique(err, "gates_society_name_uq") {
		return nil, ErrGateExists
	}
	if err != nil {
		return nil, err
	}
	s.log(ctx, soc, actor, "gate.create", "gate", g.ID, map[string]any{"name": name})
	return &g, nil
}

func (s *Service) UpdateGate(ctx context.Context, soc, actor, id uuid.UUID, name *string, active *bool) (*Gate, error) {
	var g Gate
	cur := s.pool.QueryRow(ctx, `SELECT id, name, active FROM gates WHERE id=$1 AND society_id=$2`, id, soc)
	if err := cur.Scan(&g.ID, &g.Name, &g.Active); err != nil {
		return nil, ErrGateNotFound
	}
	if name != nil {
		n, ok := cleanName(*name, 40)
		if !ok {
			return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "gate name must be 2-40 characters")
		}
		g.Name = n
	}
	if active != nil {
		g.Active = *active
	}
	if _, err := s.pool.Exec(ctx, `UPDATE gates SET name=$3, active=$4 WHERE id=$1 AND society_id=$2`, id, soc, g.Name, g.Active); err != nil {
		if database.IsUnique(err, "gates_society_name_uq") {
			return nil, ErrGateExists
		}
		return nil, err
	}
	s.log(ctx, soc, actor, "gate.update", "gate", id, map[string]any{"name": g.Name, "active": g.Active})
	return &g, nil
}

// ── gate settings ────────────────────────────────────────────────────────────

type Settings struct {
	WalkinPhotoRequired bool `json:"walkin_photo_required"`
}

func (s *Service) Settings(ctx context.Context, soc uuid.UUID) (*Settings, error) {
	var st Settings
	err := s.pool.QueryRow(ctx, `SELECT walkin_photo_required FROM societies WHERE id=$1`, soc).Scan(&st.WalkinPhotoRequired)
	return &st, err
}

func (s *Service) UpdateSettings(ctx context.Context, soc, actor uuid.UUID, photoRequired bool) (*Settings, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE societies SET walkin_photo_required=$2, updated_at=now() WHERE id=$1`, soc, photoRequired); err != nil {
		return nil, err
	}
	s.log(ctx, soc, actor, "gate.settings", "society", soc, map[string]any{"walkin_photo_required": photoRequired})
	return &Settings{WalkinPhotoRequired: photoRequired}, nil
}

// ── guards ───────────────────────────────────────────────────────────────────

type Guard struct {
	ID        uuid.UUID  `json:"id"` // membership id
	UserID    uuid.UUID  `json:"user_id"`
	FullName  string     `json:"full_name"`
	Phone     string     `json:"phone"`
	Status    string     `json:"status"` // active | suspended
	OnDuty    bool       `json:"on_duty"`
	GateName  *string    `json:"gate_name,omitempty"`
	ShiftFrom *time.Time `json:"shift_started_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

const guardSelect = `SELECT m.id, u.id, u.full_name, u.phone, m.status, (sh.id IS NOT NULL), g.name, sh.started_at, m.created_at
	FROM memberships m JOIN users u ON u.id=m.user_id
	LEFT JOIN guard_shifts sh ON sh.membership_id=m.id AND sh.ended_at IS NULL
	LEFT JOIN gates g ON g.id=sh.gate_id `

func scanGuard(row pgx.Row) (*Guard, error) {
	var g Guard
	if err := row.Scan(&g.ID, &g.UserID, &g.FullName, &g.Phone, &g.Status, &g.OnDuty, &g.GateName, &g.ShiftFrom, &g.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrGuardNotFound
		}
		return nil, err
	}
	return &g, nil
}

func (s *Service) Guards(ctx context.Context, soc uuid.UUID) ([]Guard, error) {
	rows, err := s.pool.Query(ctx, guardSelect+`WHERE m.society_id=$1 AND m.role_key='guard' AND m.status IN ('active','suspended') ORDER BY lower(u.full_name)`, soc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Guard{}
	for rows.Next() {
		g, err := scanGuard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

func (s *Service) guard(ctx context.Context, soc, id uuid.UUID) (*Guard, error) {
	return scanGuard(s.pool.QueryRow(ctx, guardSelect+`WHERE m.id=$1 AND m.society_id=$2 AND m.role_key='guard' AND m.status IN ('active','suspended')`, id, soc))
}

func (s *Service) CreateGuard(ctx context.Context, soc, actor uuid.UUID, fullName, phone string) (*Guard, error) {
	name, ok := cleanName(fullName, 80)
	if !ok {
		return nil, ErrBadName
	}
	phone = resident.NormalizePhone(phone)
	if !resident.ValidPhone(phone) {
		return nil, httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var userID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE phone=$1`, phone).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, `INSERT INTO users (full_name, phone) VALUES ($1,$2) RETURNING id`, name, phone).Scan(&userID); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id=$1 AND society_id=$2 AND status IN ('active','suspended'))`, userID, soc).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrGuardExists
	}
	var mid uuid.UUID
	// a previously removed guard is re-activated instead of duplicated
	err = tx.QueryRow(ctx, `UPDATE memberships SET status='active', role_key='guard', unit_id=NULL
		WHERE user_id=$1 AND society_id=$2 AND unit_id IS NULL AND role_key='guard' AND status='inactive' RETURNING id`, userID, soc).Scan(&mid)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'guard') RETURNING id`, userID, soc).Scan(&mid)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, soc, actor, "guard.create", "membership", mid, nil)
	return s.guard(ctx, soc, mid)
}

// UpdateGuard renames (only people who exist solely as this guard keep their name editable) or suspends/reactivates.
func (s *Service) UpdateGuard(ctx context.Context, soc, actor, id uuid.UUID, name, status *string) (*Guard, error) {
	g, err := s.guard(ctx, soc, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		n, ok := cleanName(*name, 80)
		if !ok {
			return nil, ErrBadName
		}
		if _, err := s.pool.Exec(ctx, `UPDATE users SET full_name=$2 WHERE id=$1`, g.UserID, n); err != nil {
			return nil, err
		}
	}
	if status != nil && *status != g.Status {
		switch *status {
		case "suspended":
			if err := s.cutAccess(ctx, soc, g, "suspended"); err != nil {
				return nil, err
			}
		case "active":
			if _, err := s.pool.Exec(ctx, `UPDATE memberships SET status='active' WHERE id=$1 AND society_id=$2`, id, soc); err != nil {
				return nil, err
			}
		default:
			return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "status must be active or suspended")
		}
	}
	s.log(ctx, soc, actor, "guard.update", "membership", id, map[string]any{"status": status})
	return s.guard(ctx, soc, id)
}

func (s *Service) RemoveGuard(ctx context.Context, soc, actor, id uuid.UUID) error {
	g, err := s.guard(ctx, soc, id)
	if err != nil {
		return err
	}
	if err := s.cutAccess(ctx, soc, g, "inactive"); err != nil {
		return err
	}
	s.log(ctx, soc, actor, "guard.remove", "membership", id, nil)
	return nil
}

// cutAccess ends the shift, revokes refresh tokens and drops live connections right away.
func (s *Service) cutAccess(ctx context.Context, soc uuid.UUID, g *Guard, to string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE memberships SET status=$3 WHERE id=$1 AND society_id=$2`, g.ID, soc, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE guard_shifts SET ended_at=now() WHERE membership_id=$1 AND ended_at IS NULL`, g.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE membership_id=$1 AND revoked_at IS NULL`, g.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.hub != nil {
		s.hub.Publish(ctx, realtime.Event{Type: "session.revoked", SocietyID: soc, User: &g.UserID})
	}
	return nil
}

// ── shifts ───────────────────────────────────────────────────────────────────

type Shift struct {
	ID        uuid.UUID  `json:"id"`
	GateID    uuid.UUID  `json:"gate_id"`
	GateName  string     `json:"gate_name"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

func (s *Service) CurrentShift(ctx context.Context, soc, membership uuid.UUID) (*Shift, error) {
	var sh Shift
	err := s.pool.QueryRow(ctx, `SELECT sh.id, sh.gate_id, g.name, sh.started_at FROM guard_shifts sh JOIN gates g ON g.id=sh.gate_id
		WHERE sh.membership_id=$1 AND sh.society_id=$2 AND sh.ended_at IS NULL`, membership, soc).Scan(&sh.ID, &sh.GateID, &sh.GateName, &sh.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &sh, err
}

// StartShift begins a shift at a gate; an open shift is closed first (switching gates).
func (s *Service) StartShift(ctx context.Context, soc, membership, gateID uuid.UUID) (*Shift, error) {
	var name string
	var active bool
	if err := s.pool.QueryRow(ctx, `SELECT name, active FROM gates WHERE id=$1 AND society_id=$2`, gateID, soc).Scan(&name, &active); err != nil {
		return nil, ErrGateNotFound
	}
	if !active {
		return nil, ErrGateInactive
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE guard_shifts SET ended_at=now() WHERE membership_id=$1 AND ended_at IS NULL`, membership); err != nil {
		return nil, err
	}
	sh := Shift{GateID: gateID, GateName: name}
	if err := tx.QueryRow(ctx, `INSERT INTO guard_shifts (society_id, membership_id, gate_id) VALUES ($1,$2,$3) RETURNING id, started_at`, soc, membership, gateID).Scan(&sh.ID, &sh.StartedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &sh, nil
}

func (s *Service) EndShift(ctx context.Context, soc, membership uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE guard_shifts SET ended_at=now() WHERE membership_id=$1 AND society_id=$2 AND ended_at IS NULL`, membership, soc)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoShift
	}
	return nil
}

// SweepShifts closes shifts left open for 16 hours.
func (s *Service) SweepShifts(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE guard_shifts SET ended_at=now() WHERE ended_at IS NULL AND started_at < now() - interval '16 hours'`)
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
			_, _ = s.SweepShifts(ctx)
		}
	}
}
