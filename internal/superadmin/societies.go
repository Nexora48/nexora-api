package superadmin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/limits"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

var (
	ErrSocietyNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "society not found")
	ErrSlugTaken       = httpx.NewError(http.StatusConflict, "SLUG_TAKEN", "this society handle is already used")
	ErrEmailTaken      = httpx.NewError(http.StatusConflict, "EMAIL_TAKEN", "this email belongs to another user")
	ErrAlreadyAdmin    = httpx.NewError(http.StatusConflict, "ALREADY_ADMIN", "this person is already an admin of the society")
	ErrAdminNotFound   = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "admin not found in this society")
	ErrAlreadySusp     = httpx.NewError(http.StatusConflict, "ALREADY_SUSPENDED", "the society is already suspended")
	ErrNotSuspended    = httpx.NewError(http.StatusConflict, "NOT_SUSPENDED", "the society is not suspended")
	ErrFeatureUnknown  = httpx.NewError(http.StatusBadRequest, "FEATURE_UNKNOWN", "unknown service key")
	ErrInvalidPhone    = httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
)

// ── Types ────────────────────────────────────────────────────────────────────

type SocietyRow struct {
	ID              uuid.UUID  `json:"id"`
	Name            string     `json:"name"`
	Slug            string     `json:"slug"`
	City            string     `json:"city"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	Blocks          int        `json:"blocks"`
	Flats           int        `json:"flats"`
	Residents       int        `json:"residents"`
	LastSignIn      *time.Time `json:"last_sign_in,omitempty"`
	EnabledServices int        `json:"enabled_services"`
	TotalServices   int        `json:"total_services"`
}

type AdminRow struct {
	ID                 uuid.UUID  `json:"id"`
	FullName           string     `json:"full_name"`
	Email              *string    `json:"email,omitempty"`
	Phone              string     `json:"phone"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	MustChangePassword bool       `json:"must_change_password"`
}

type FeatureRow struct {
	Key            string     `json:"key"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	DefaultEnabled bool       `json:"default_enabled"`
	Override       *bool      `json:"override"` // nil = follows the default
	Enabled        bool       `json:"enabled"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type SocietyDetail struct {
	SocietyRow
	Address         string       `json:"address"`
	State           string       `json:"state"`
	Pincode         string       `json:"pincode"`
	SuspendedReason *string      `json:"suspended_reason,omitempty"`
	SuspendedAt     *time.Time   `json:"suspended_at,omitempty"`
	Admins          []AdminRow   `json:"admins"`
	Features        []FeatureRow `json:"features"`
	Recent          []AuditRow   `json:"recent_activity"`
}

type AdminInput struct {
	FullName string
	Email    string
	Phone    string
}

type CreateSocietyInput struct {
	Name, Slug, Address, City, State, Pincode, Status string
	Admin                                             AdminInput
}

// Credentials is returned once when an admin account is created or reset.
type Credentials struct {
	UserID            uuid.UUID `json:"user_id"`
	Email             string    `json:"email"`
	Phone             string    `json:"phone"`
	TemporaryPassword *string   `json:"temporary_password,omitempty"` // nil: existing account keeps its password
	ExistingAccount   bool      `json:"existing_account"`
}

// ── Helpers ──────────────────────────────────────────────────────────────────

const pwAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// tempPassword returns 12 chars containing letters and digits (satisfies the password policy).
func tempPassword() (string, error) {
	for {
		b := make([]byte, 12)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pwAlphabet))))
			if err != nil {
				return "", err
			}
			b[i] = pwAlphabet[n.Int64()]
		}
		if auth.ValidPasswordPolicy(string(b)) {
			return string(b), nil
		}
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "society"
	}
	return s
}

func (s *Service) platformLog(ctx context.Context, actor uuid.UUID, society *uuid.UUID, action, entity, entityID string, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &actor, ActorType: "platform", SocietyID: society, Action: action, Entity: entity, EntityID: entityID, Meta: meta})
}

// ── List / detail ────────────────────────────────────────────────────────────

func (s *Service) ListSocieties(ctx context.Context, q, status string, limit, offset int) ([]SocietyRow, int, error) {
	args := []any{}
	where := []string{"true"}
	if q != "" {
		args = append(args, "%"+q+"%")
		where = append(where, fmt.Sprintf("(s.name ILIKE $%d OR s.slug ILIKE $%d OR s.city ILIKE $%d)", len(args), len(args), len(args)))
	}
	if status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("s.status = $%d", len(args)))
	}
	args = append(args, limit, offset)
	rows, err := s.store.pool.Query(ctx, `
		SELECT s.id, s.name, s.slug, s.city, s.status, s.created_at,
		  (SELECT count(*) FROM blocks b WHERE b.society_id=s.id),
		  (SELECT count(*) FROM units u WHERE u.society_id=s.id),
		  (SELECT count(*) FROM memberships m WHERE m.society_id=s.id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		  (SELECT max(us.last_login_at) FROM memberships m JOIN users us ON us.id=m.user_id WHERE m.society_id=s.id),
		  (SELECT count(*) FROM features f LEFT JOIN society_features sf ON sf.feature_key=f.key AND sf.society_id=s.id WHERE COALESCE(sf.enabled, f.default_enabled)),
		  (SELECT count(*) FROM features),
		  count(*) OVER()
		FROM societies s WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY s.created_at DESC, s.id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SocietyRow{}
	total := 0
	for rows.Next() {
		var r SocietyRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Slug, &r.City, &r.Status, &r.CreatedAt, &r.Blocks, &r.Flats, &r.Residents,
			&r.LastSignIn, &r.EnabledServices, &r.TotalServices, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *Service) GetSociety(ctx context.Context, id uuid.UUID) (*SocietyDetail, error) {
	rows, _, err := s.ListSocietiesByID(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &SocietyDetail{SocietyRow: *rows, Admins: []AdminRow{}, Features: []FeatureRow{}, Recent: []AuditRow{}}
	if err := s.store.pool.QueryRow(ctx, `SELECT address, state, pincode, suspended_reason, suspended_at FROM societies WHERE id=$1`, id).
		Scan(&d.Address, &d.State, &d.Pincode, &d.SuspendedReason, &d.SuspendedAt); err != nil {
		return nil, err
	}
	ar, err := s.store.pool.Query(ctx, `
		SELECT u.id, u.full_name, u.email, u.phone, u.last_login_at, u.must_change_password
		FROM memberships m JOIN users u ON u.id=m.user_id
		WHERE m.society_id=$1 AND m.role_key='society_admin' AND m.status='active' ORDER BY u.full_name`, id)
	if err != nil {
		return nil, err
	}
	for ar.Next() {
		var a AdminRow
		if err := ar.Scan(&a.ID, &a.FullName, &a.Email, &a.Phone, &a.LastLoginAt, &a.MustChangePassword); err != nil {
			ar.Close()
			return nil, err
		}
		d.Admins = append(d.Admins, a)
	}
	ar.Close()
	if d.Features, err = s.societyFeatures(ctx, id); err != nil {
		return nil, err
	}
	d.Recent, _, err = s.ListAudit(ctx, AuditFilter{SocietyID: &id, Limit: 10})
	return d, err
}

// ListSocietiesByID returns the single row used as the header of the detail view.
func (s *Service) ListSocietiesByID(ctx context.Context, id uuid.UUID) (*SocietyRow, int, error) {
	var r SocietyRow
	err := s.store.pool.QueryRow(ctx, `
		SELECT s.id, s.name, s.slug, s.city, s.status, s.created_at,
		  (SELECT count(*) FROM blocks b WHERE b.society_id=s.id),
		  (SELECT count(*) FROM units u WHERE u.society_id=s.id),
		  (SELECT count(*) FROM memberships m WHERE m.society_id=s.id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		  (SELECT max(us.last_login_at) FROM memberships m JOIN users us ON us.id=m.user_id WHERE m.society_id=s.id),
		  (SELECT count(*) FROM features f LEFT JOIN society_features sf ON sf.feature_key=f.key AND sf.society_id=s.id WHERE COALESCE(sf.enabled, f.default_enabled)),
		  (SELECT count(*) FROM features)
		FROM societies s WHERE s.id=$1`, id).
		Scan(&r.ID, &r.Name, &r.Slug, &r.City, &r.Status, &r.CreatedAt, &r.Blocks, &r.Flats, &r.Residents, &r.LastSignIn, &r.EnabledServices, &r.TotalServices)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrSocietyNotFound
	}
	return &r, 1, err
}

// ── Onboarding & edit ────────────────────────────────────────────────────────

func (s *Service) CreateSociety(ctx context.Context, actor uuid.UUID, in CreateSocietyInput) (*SocietyDetail, *Credentials, error) {
	in.Name = strings.TrimSpace(in.Name)
	explicit := strings.TrimSpace(in.Slug) != ""
	slug := slugify(in.Slug)
	if !explicit {
		slug = slugify(in.Name)
	}
	if in.Status == "" {
		in.Status = "active"
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var id uuid.UUID
	for attempt := 0; attempt < 20; attempt++ {
		candidate := slug
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", slug, attempt+1)
		}
		// savepoint so a unique violation doesn't abort the transaction
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		err = sp.QueryRow(ctx, `INSERT INTO societies (name, slug, address, city, state, pincode, status, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			in.Name, candidate, strings.TrimSpace(in.Address), strings.TrimSpace(in.City), strings.TrimSpace(in.State), strings.TrimSpace(in.Pincode), in.Status, actor).Scan(&id)
		if database.IsUnique(err, "") {
			_ = sp.Rollback(ctx)
			if explicit {
				return nil, nil, ErrSlugTaken
			}
			continue
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			return nil, nil, err
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, nil, err
		}
		slug = candidate
		break
	}
	if id == uuid.Nil {
		return nil, nil, ErrSlugTaken
	}

	cred, err := s.provisionAdmin(ctx, tx, id, in.Admin)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	s.platformLog(ctx, actor, &id, "society.create", "society", id.String(), map[string]any{"name": in.Name, "slug": slug, "status": in.Status, "admin_email": cred.Email})
	d, err := s.GetSociety(ctx, id)
	return d, cred, err
}

// provisionAdmin makes the person a society_admin; new accounts get a one-time temporary password.
func (s *Service) provisionAdmin(ctx context.Context, tx pgx.Tx, societyID uuid.UUID, in AdminInput) (*Credentials, error) {
	phone := resident.NormalizePhone(in.Phone)
	if !resident.ValidPhone(phone) {
		return nil, ErrInvalidPhone
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	cred := &Credentials{Phone: phone}

	var userID uuid.UUID
	var existingEmail, existingHash *string
	err := tx.QueryRow(ctx, `SELECT id, email, password_hash FROM users WHERE phone=$1`, phone).Scan(&userID, &existingEmail, &existingHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		pw, err := tempPassword()
		if err != nil {
			return nil, err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return nil, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO users (full_name, phone, email, password_hash, must_change_password) VALUES ($1,$2,$3,$4,true) RETURNING id`,
			strings.TrimSpace(in.FullName), phone, email, hash).Scan(&userID)
		if database.IsUnique(err, "") {
			return nil, ErrEmailTaken
		}
		if err != nil {
			return nil, err
		}
		cred.Email, cred.TemporaryPassword = email, &pw
	case err != nil:
		return nil, err
	default: // the person already has an account (e.g. a resident elsewhere): reuse it
		cred.ExistingAccount = true
		cred.Email = email
		if existingEmail != nil {
			cred.Email = *existingEmail
		} else {
			if _, err := tx.Exec(ctx, `UPDATE users SET email=$2 WHERE id=$1`, userID, email); err != nil {
				if database.IsUnique(err, "") {
					return nil, ErrEmailTaken
				}
				return nil, err
			}
		}
		if existingHash == nil {
			pw, err := tempPassword()
			if err != nil {
				return nil, err
			}
			hash, err := auth.HashPassword(pw)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, must_change_password=true WHERE id=$1`, userID, hash); err != nil {
				return nil, err
			}
			cred.TemporaryPassword = &pw
		}
	}
	cred.UserID = userID

	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE user_id=$1 AND society_id=$2 AND role_key='society_admin' AND status='active'`, userID, societyID).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrAlreadyAdmin
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'society_admin')`, userID, societyID); err != nil {
		return nil, err
	}
	return cred, nil
}

type UpdateSocietyInput struct{ Name, Address, City, State, Pincode string }

func (s *Service) UpdateSociety(ctx context.Context, actor, id uuid.UUID, in UpdateSocietyInput) (*SocietyDetail, error) {
	tag, err := s.store.pool.Exec(ctx, `UPDATE societies SET name=$2, address=$3, city=$4, state=$5, pincode=$6, updated_at=now() WHERE id=$1`,
		id, strings.TrimSpace(in.Name), strings.TrimSpace(in.Address), strings.TrimSpace(in.City), strings.TrimSpace(in.State), strings.TrimSpace(in.Pincode))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrSocietyNotFound
	}
	s.platformLog(ctx, actor, &id, "society.update", "society", id.String(), map[string]any{"name": in.Name})
	return s.GetSociety(ctx, id)
}

// ── Suspend / reactivate ─────────────────────────────────────────────────────

func (s *Service) Suspend(ctx context.Context, actor, id uuid.UUID, reason string) error {
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM societies WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSocietyNotFound
		}
		return err
	}
	if status == "suspended" {
		return ErrAlreadySusp
	}
	if _, err := tx.Exec(ctx, `UPDATE societies SET status='suspended', suspended_reason=$2, suspended_at=now(), updated_at=now() WHERE id=$1`, id, reason); err != nil {
		return err
	}
	// kill every session of the society right away
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE revoked_at IS NULL
		AND membership_id IN (SELECT id FROM memberships WHERE society_id=$1)`, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.platformLog(ctx, actor, &id, "society.suspend", "society", id.String(), map[string]any{"from": status, "reason": reason})
	return nil
}

func (s *Service) Reactivate(ctx context.Context, actor, id uuid.UUID) error {
	var status string
	if err := s.store.pool.QueryRow(ctx, `SELECT status FROM societies WHERE id=$1`, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSocietyNotFound
		}
		return err
	}
	if status != "suspended" {
		return ErrNotSuspended
	}
	if _, err := s.store.pool.Exec(ctx, `UPDATE societies SET status='active', suspended_reason=NULL, suspended_at=NULL, updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	s.platformLog(ctx, actor, &id, "society.reactivate", "society", id.String(), nil)
	return nil
}

// ── Admins ───────────────────────────────────────────────────────────────────

func (s *Service) AddAdmin(ctx context.Context, actor, societyID uuid.UUID, in AdminInput) (*Credentials, error) {
	var exists bool
	if err := s.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM societies WHERE id=$1)`, societyID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrSocietyNotFound
	}
	if err := limits.New(s.store.pool).Check(ctx, societyID, limits.MaxAdmins, 1); err != nil {
		return nil, err
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	cred, err := s.provisionAdmin(ctx, tx, societyID, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, &societyID, "admin.create", "user", cred.UserID.String(), map[string]any{"email": cred.Email})
	return cred, nil
}

func (s *Service) ResetAdminPassword(ctx context.Context, actor, societyID, userID uuid.UUID) (*Credentials, error) {
	var email *string
	var phone string
	err := s.store.pool.QueryRow(ctx, `SELECT u.email, u.phone FROM users u JOIN memberships m ON m.user_id=u.id
		WHERE u.id=$1 AND m.society_id=$2 AND m.role_key='society_admin' AND m.status='active'`, userID, societyID).Scan(&email, &phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, err
	}
	pw, err := tempPassword()
	if err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return nil, err
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, must_change_password=true, updated_at=now() WHERE id=$1`, userID, hash); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, &societyID, "admin.reset_password", "user", userID.String(), nil)
	cred := &Credentials{UserID: userID, Phone: phone, TemporaryPassword: &pw, ExistingAccount: true}
	if email != nil {
		cred.Email = *email
	}
	return cred, nil
}

// ── Audit ────────────────────────────────────────────────────────────────────

type AuditRow struct {
	ID          int64           `json:"id"`
	ActorType   string          `json:"actor_type"`
	ActorID     *uuid.UUID      `json:"actor_id,omitempty"`
	ActorName   *string         `json:"actor_name,omitempty"`
	ActorEmail  *string         `json:"actor_email,omitempty"`
	ActorRole   *string         `json:"actor_role,omitempty"`
	SocietyID   *uuid.UUID      `json:"society_id,omitempty"`
	SocietyName *string         `json:"society_name,omitempty"`
	Action      string          `json:"action"`
	Entity      string          `json:"entity"`
	EntityID    string          `json:"entity_id"`
	Meta        json.RawMessage `json:"meta"`
	IP          string          `json:"ip"`
	CreatedAt   time.Time       `json:"created_at"`
}

type AuditFilter struct {
	SocietyID *uuid.UUID
	ActorID   *uuid.UUID
	ActorType string // user | platform | system
	Action    string // exact or prefix ending with '*', e.g. "feature.*"
	Entity    string
	Text      string // matches entity id, action or entity (substring)
	From, To  *time.Time
	Limit     int
	Offset    int
}

func (f AuditFilter) where() (string, []any) {
	args := []any{}
	where := []string{"true"}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.SocietyID != nil {
		add("a.society_id = ?", *f.SocietyID)
	}
	if f.ActorID != nil {
		add("a.actor_id = ?", *f.ActorID)
	}
	if f.ActorType != "" {
		add("a.actor_type = ?", f.ActorType)
	}
	if f.Entity != "" {
		add("a.entity = ?", f.Entity)
	}
	if f.Text != "" {
		add("(a.entity_id = ? OR a.action ILIKE '%' || ? || '%' OR a.entity ILIKE '%' || ? || '%')", f.Text)
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, "*") {
			add("a.action LIKE ?", strings.TrimSuffix(f.Action, "*")+"%")
		} else {
			add("a.action = ?", f.Action)
		}
	}
	if f.From != nil {
		add("a.created_at >= ?", *f.From)
	}
	if f.To != nil {
		add("a.created_at < ?", *f.To)
	}
	return strings.Join(where, " AND "), args
}

const auditSelect = `
	SELECT a.id, a.actor_type, a.actor_id, COALESCE(pu.full_name, uu.full_name), COALESCE(pu.email, uu.email), pu.role, a.society_id, so.name,
	       a.action, a.entity, a.entity_id, a.meta, a.ip, a.created_at`

const auditFrom = `
	FROM audit_logs a
	LEFT JOIN platform_users pu ON pu.id = a.actor_id AND a.actor_type='platform'
	LEFT JOIN users uu ON uu.id = a.actor_id AND a.actor_type='user'
	LEFT JOIN societies so ON so.id = a.society_id`

func scanAudit(row pgx.Row, extra ...any) (AuditRow, error) {
	var r AuditRow
	dst := append([]any{&r.ID, &r.ActorType, &r.ActorID, &r.ActorName, &r.ActorEmail, &r.ActorRole, &r.SocietyID, &r.SocietyName, &r.Action, &r.Entity, &r.EntityID, &r.Meta, &r.IP, &r.CreatedAt}, extra...)
	err := row.Scan(dst...)
	return r, err
}

func (s *Service) ListAudit(ctx context.Context, f AuditFilter) ([]AuditRow, int, error) {
	cond, args := f.where()
	args = append(args, f.Limit, f.Offset)
	rows, err := s.store.pool.Query(ctx, auditSelect+`, count(*) OVER()`+auditFrom+` WHERE `+cond+
		fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditRow{}
	total := 0
	for rows.Next() {
		r, err := scanAudit(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetAudit returns one audit row with the actor's details.
func (s *Service) GetAudit(ctx context.Context, id int64) (*AuditRow, error) {
	r, err := scanAudit(s.store.pool.QueryRow(ctx, auditSelect+auditFrom+` WHERE a.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAuditNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ── Stats ────────────────────────────────────────────────────────────────────

type PlatformStats struct {
	Societies          int `json:"societies"`
	Active             int `json:"active"`
	Trial              int `json:"trial"`
	Suspended          int `json:"suspended"`
	Flats              int `json:"flats"`
	Residents          int `json:"residents"`
	Admins             int `json:"admins"`
	ServicesDisabledAt int `json:"services_disabled_overrides"`
}

func (s *Service) Stats(ctx context.Context) (*PlatformStats, error) {
	var st PlatformStats
	err := s.store.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status='active'), count(*) FILTER (WHERE status='trial'), count(*) FILTER (WHERE status='suspended'),
		  (SELECT count(*) FROM units),
		  (SELECT count(*) FROM memberships WHERE status='active' AND role_key IN ('resident_owner','resident_tenant','family_member')),
		  (SELECT count(*) FROM memberships WHERE status='active' AND role_key='society_admin'),
		  (SELECT count(*) FROM society_features sf JOIN features f ON f.key=sf.feature_key WHERE sf.enabled <> f.default_enabled AND NOT sf.enabled)
		FROM societies`).Scan(&st.Societies, &st.Active, &st.Trial, &st.Suspended, &st.Flats, &st.Residents, &st.Admins, &st.ServicesDisabledAt)
	return &st, err
}
