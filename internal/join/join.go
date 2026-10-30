// Package join is self-registration for the resident app. Someone who verified a phone number that belongs to no
// society picks a society (by its join code) and a flat and asks to join; the office approves or rejects. Approval
// creates the resident exactly like "Add resident" does, so every rule (one owner per flat, plan limits, audit) applies.
// Residents added by the office never pass through here.
package join

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

var (
	ErrCodeUnknown   = httpx.NewError(http.StatusNotFound, "JOIN_CODE_UNKNOWN", "no society matches this code")
	ErrJoinOff       = httpx.NewError(http.StatusForbidden, "JOIN_DISABLED", "this society does not accept join requests; ask the office to add you")
	ErrAlreadyMember = httpx.NewError(http.StatusConflict, "ALREADY_MEMBER", "you already belong to this society; sign in with your number")
	ErrAlreadyOpen   = httpx.NewError(http.StatusConflict, "REQUEST_PENDING", "you already have a request waiting for this society")
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "request not found")
	ErrNotPending    = httpx.NewError(http.StatusConflict, "NOT_PENDING", "this request has already been decided")
	ErrFlatNotFound  = httpx.NewError(http.StatusNotFound, "UNIT_NOT_FOUND", "flat not found in this society")
	ErrFlatTaken     = httpx.NewError(http.StatusConflict, "FLAT_TAKEN", "this flat already has a resident of that type; choose another flat or type")
)

func invalid(msg string) *httpx.AppError {
	return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg)
}

type Service struct {
	pool  *pgxpool.Pool
	res   *resident.Service
	notes *notify.Service
	audit *audit.Logger
}

func New(pool *pgxpool.Pool, res *resident.Service, notes *notify.Service, a *audit.Logger) *Service {
	return &Service{pool: pool, res: res, notes: notes, audit: a}
}

// ── applicant side (token = a verified phone number) ─────────────────────────

type Unit struct {
	ID          uuid.UUID `json:"id"`
	Number      string    `json:"number"`
	UnitType    string    `json:"unit_type"`    // 1BHK, 2BHK, ...
	OwnerTaken  bool      `json:"owner_taken"`  // the flat already has its owner
	TenantTaken bool      `json:"tenant_taken"` // the flat already has its tenant
}
type Block struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Units []Unit `json:"units"`
}
type Society struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	City   string    `json:"city"`
	Blocks []Block   `json:"blocks"`
}

func normCode(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// societyBy picks the lookup column: a society id (from the picker) or the join code.
func societyBy(code, id string) string {
	if strings.TrimSpace(id) != "" {
		return "id::text=$1"
	}
	return "join_code=$1"
}

func societyArg(code, id string) string {
	if v := strings.TrimSpace(id); v != "" {
		return strings.ToLower(v)
	}
	return normCode(code)
}

// Listed is a society an applicant can pick.
type Listed struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	City string    `json:"city"`
}

// Societies lists the societies that accept sign-ups, for the picker.
func (s *Service) Societies(ctx context.Context) ([]Listed, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, city FROM societies WHERE join_enabled AND status <> 'suspended' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Listed{}
	for rows.Next() {
		var l Listed
		if err := rows.Scan(&l.ID, &l.Name, &l.City); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Lookup returns the society behind a join code with its blocks and flats.
func (s *Service) Lookup(ctx context.Context, code, id string) (*Society, error) {
	var so Society
	var on bool
	var status string
	err := s.pool.QueryRow(ctx, `SELECT id, name, city, join_enabled, status FROM societies WHERE `+societyBy(code, id), societyArg(code, id)).Scan(&so.ID, &so.Name, &so.City, &on, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status == "suspended") {
		return nil, ErrCodeUnknown
	}
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrJoinOff
	}
	rows, err := s.pool.Query(ctx, `SELECT b.code, b.name, u.id, u.number, u.unit_type,
		EXISTS (SELECT 1 FROM memberships m WHERE m.unit_id=u.id AND m.role_key='resident_owner' AND m.status IN ('active','suspended')),
		EXISTS (SELECT 1 FROM memberships m WHERE m.unit_id=u.id AND m.role_key='resident_tenant' AND m.status IN ('active','suspended'))
		FROM blocks b JOIN units u ON u.block_id=b.id WHERE b.society_id=$1 ORDER BY b.sort_order, b.code, u.floor, u.number`, so.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	so.Blocks = []Block{}
	for rows.Next() {
		var bc, bn, num, ut string
		var id uuid.UUID
		var ot, tt bool
		if err := rows.Scan(&bc, &bn, &id, &num, &ut, &ot, &tt); err != nil {
			return nil, err
		}
		if n := len(so.Blocks); n == 0 || so.Blocks[n-1].Code != bc {
			so.Blocks = append(so.Blocks, Block{Code: bc, Name: bn, Units: []Unit{}})
		}
		b := &so.Blocks[len(so.Blocks)-1]
		b.Units = append(b.Units, Unit{ID: id, Number: num, UnitType: ut, OwnerTaken: ot, TenantTaken: tt})
	}
	return &so, rows.Err()
}

type RequestInput struct {
	Code      string `json:"code" validate:"omitempty,max=20"`
	SocietyID string `json:"society_id" validate:"omitempty,uuid"`
	UnitID    string `json:"unit_id" validate:"required,uuid"`
	Type      string `json:"type" validate:"required,oneof=owner tenant"`
	FullName  string `json:"full_name" validate:"required,min=2,max=100"`
}

// Request files a join request for the verified phone.
func (s *Service) Request(ctx context.Context, phone string, in RequestInput) (*Status, error) {
	name := strings.TrimSpace(in.FullName)
	if len(name) < 2 {
		return nil, invalid("enter your full name")
	}
	if strings.TrimSpace(in.Code) == "" && strings.TrimSpace(in.SocietyID) == "" {
		return nil, invalid("choose your society")
	}
	var soc uuid.UUID
	var on bool
	var status, socName string
	err := s.pool.QueryRow(ctx, `SELECT id, join_enabled, status, name FROM societies WHERE `+societyBy(in.Code, in.SocietyID), societyArg(in.Code, in.SocietyID)).Scan(&soc, &on, &status, &socName)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status == "suspended") {
		return nil, ErrCodeUnknown
	}
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrJoinOff
	}
	unit, _ := uuid.Parse(in.UnitID)
	var flat string
	if err := s.pool.QueryRow(ctx, `SELECT b.code || '-' || u.number FROM units u JOIN blocks b ON b.id=u.block_id WHERE u.id=$1 AND u.society_id=$2`, unit, soc).Scan(&flat); err != nil {
		return nil, ErrFlatNotFound
	}
	role := "resident_tenant"
	if in.Type == "owner" {
		role = "resident_owner"
	}
	var taken bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE unit_id=$1 AND role_key=$2 AND status IN ('active','suspended'))`, unit, role).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, ErrFlatTaken
	}
	var member bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id=m.user_id WHERE u.phone=$1 AND m.society_id=$2 AND m.status IN ('active','suspended'))`, phone, soc).Scan(&member); err != nil {
		return nil, err
	}
	if member {
		return nil, ErrAlreadyMember
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `INSERT INTO join_requests (society_id, unit_id, phone, full_name, requested_type) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		soc, unit, phone, name, in.Type).Scan(&id)
	if database.IsUnique(err, "join_requests_pending_uq") {
		return nil, ErrAlreadyOpen
	}
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{SocietyID: &soc, Action: "join.request", Entity: "join_request", EntityID: id.String(), Meta: map[string]any{"flat": flat, "type": in.Type}})
	if users := s.notes.ByRoles(ctx, soc, "society_admin"); len(users) > 0 {
		s.notes.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: "join.request", Title: "New join request",
			Body: fmt.Sprintf("%s wants to join flat %s as %s", name, flat, in.Type), Data: map[string]any{"type": "join", "id": id.String()}, Priority: notify.PriorityNormal})
	}
	return s.statusOf(ctx, phone)
}

type Status struct {
	State       string     `json:"state"` // none | pending | approved | rejected
	ID          *uuid.UUID `json:"id,omitempty"`
	Society     string     `json:"society,omitempty"`
	Flat        string     `json:"flat,omitempty"`
	FlatType    string     `json:"flat_type,omitempty"`
	Type        string     `json:"type,omitempty"`
	FullName    string     `json:"full_name,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	// Member is true once the phone belongs to a society (approved or added by the office): the app can sign in.
	Member bool `json:"member"`
}

// StatusOf is the answer for the waiting screen: the latest request of this phone.
func (s *Service) StatusOf(ctx context.Context, phone string) (*Status, error) {
	return s.statusOf(ctx, phone)
}

func (s *Service) statusOf(ctx context.Context, phone string) (*Status, error) {
	st := &Status{State: "none"}
	_ = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id=m.user_id WHERE u.phone=$1 AND m.status='active' AND m.app_access)`, phone).Scan(&st.Member)
	var id uuid.UUID
	var reason *string
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.status, so.name, b.code || '-' || u.number, u.unit_type, r.requested_type, r.full_name, r.reason, r.created_at, r.decided_at
		FROM join_requests r JOIN societies so ON so.id=r.society_id JOIN units u ON u.id=r.unit_id JOIN blocks b ON b.id=u.block_id
		WHERE r.phone=$1 AND r.status <> 'cancelled' ORDER BY r.created_at DESC LIMIT 1`, phone).
		Scan(&id, &st.State, &st.Society, &st.Flat, &st.FlatType, &st.Type, &st.FullName, &reason, &st.RequestedAt, &st.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.ID = &id
	if reason != nil {
		st.Reason = *reason
	}
	return st, nil
}

// Cancel withdraws the caller's open request.
func (s *Service) Cancel(ctx context.Context, phone string) error {
	_, err := s.pool.Exec(ctx, `UPDATE join_requests SET status='cancelled', decided_at=now() WHERE phone=$1 AND status='pending'`, phone)
	return err
}

// ── office side ──────────────────────────────────────────────────────────────

type Row struct {
	ID        uuid.UUID  `json:"id"`
	FullName  string     `json:"full_name"`
	Phone     string     `json:"phone"`
	Flat      string     `json:"flat"`
	UnitType  string     `json:"unit_type"`
	UnitID    uuid.UUID  `json:"unit_id"`
	Type      string     `json:"type"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	DecidedBy *string    `json:"decided_by,omitempty"`
}

// List shows the society's requests; status is pending, approved, rejected or "" for all.
func (s *Service) List(ctx context.Context, soc uuid.UUID, status string, limit, offset int) ([]Row, int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.full_name, r.phone, b.code || '-' || u.number, u.unit_type, r.unit_id, r.requested_type, r.status, COALESCE(r.reason,''), r.created_at, r.decided_at, du.full_name, count(*) OVER()
		FROM join_requests r JOIN units u ON u.id=r.unit_id JOIN blocks b ON b.id=u.block_id
		LEFT JOIN memberships dm ON dm.id=r.decided_by LEFT JOIN users du ON du.id=dm.user_id
		WHERE r.society_id=$1 AND r.status <> 'cancelled' AND ($2 = '' OR r.status = $2)
		ORDER BY (r.status='pending') DESC, r.created_at DESC LIMIT $3 OFFSET $4`, soc, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Row{}
	total := 0
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.FullName, &r.Phone, &r.Flat, &r.UnitType, &r.UnitID, &r.Type, &r.Status, &r.Reason, &r.CreatedAt, &r.DecidedAt, &r.DecidedBy, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *Service) load(ctx context.Context, soc, id uuid.UUID) (*Row, error) {
	rows, _, err := s.List(ctx, soc, "", 1000, 0)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, ErrNotFound
}

// Approve creates the resident (the same code path as "Add resident") and closes the request.
func (s *Service) Approve(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Row, error) {
	r, err := s.load(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if r.Status != "pending" {
		return nil, ErrNotPending
	}
	if _, err := s.res.Create(ctx, p.SocietyID, p.UserID, resident.CreateInput{FullName: r.FullName, Phone: r.Phone, UnitID: r.UnitID, Type: r.Type}); err != nil {
		return nil, err // for example UNIT_ROLE_TAKEN: the request stays open so the office can reject it or fix the flat
	}
	tag, err := s.pool.Exec(ctx, `UPDATE join_requests SET status='approved', decided_by=$3, decided_at=now() WHERE id=$1 AND society_id=$2 AND status='pending'`, id, p.SocietyID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotPending
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "join.approve", Entity: "join_request", EntityID: id.String(), Meta: map[string]any{"flat": r.Flat, "type": r.Type}})
	return s.load(ctx, p.SocietyID, id)
}

// Reject closes the request with an optional reason the applicant will see.
func (s *Service) Reject(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Row, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		return nil, invalid("the reason can be 300 characters at most")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE join_requests SET status='rejected', reason=NULLIF($3,''), decided_by=$4, decided_at=now() WHERE id=$1 AND society_id=$2 AND status='pending'`,
		id, p.SocietyID, reason, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		if _, lerr := s.load(ctx, p.SocietyID, id); lerr != nil {
			return nil, ErrNotFound
		}
		return nil, ErrNotPending
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "join.reject", Entity: "join_request", EntityID: id.String(), Meta: map[string]any{"reason": reason}})
	return s.load(ctx, p.SocietyID, id)
}

type Settings struct {
	JoinCode string `json:"join_code"`
	Enabled  bool   `json:"enabled"`
	Pending  int    `json:"pending"`
}

func (s *Service) Settings(ctx context.Context, soc uuid.UUID) (*Settings, error) {
	var st Settings
	err := s.pool.QueryRow(ctx, `SELECT join_code, join_enabled, (SELECT count(*) FROM join_requests WHERE society_id=$1 AND status='pending') FROM societies WHERE id=$1`, soc).
		Scan(&st.JoinCode, &st.Enabled, &st.Pending)
	return &st, err
}

type SettingsInput struct {
	Enabled    *bool `json:"enabled"`
	Regenerate bool  `json:"regenerate"`
}

// UpdateSettings switches self-registration on or off and can replace the code (the old one stops working at once).
func (s *Service) UpdateSettings(ctx context.Context, p authctx.Principal, in SettingsInput) (*Settings, error) {
	if in.Enabled != nil {
		if _, err := s.pool.Exec(ctx, `UPDATE societies SET join_enabled=$2 WHERE id=$1`, p.SocietyID, *in.Enabled); err != nil {
			return nil, err
		}
	}
	if in.Regenerate {
		if _, err := s.pool.Exec(ctx, `UPDATE societies SET join_code = upper(substr(md5(gen_random_uuid()::text), 1, 8)) WHERE id=$1`, p.SocietyID); err != nil {
			return nil, err
		}
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "join.settings", Entity: "society", EntityID: p.SocietyID.String(),
		Meta: map[string]any{"enabled": in.Enabled, "regenerated": in.Regenerate}})
	return s.Settings(ctx, p.SocietyID)
}
