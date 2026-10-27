package superadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/auth"
	nxmail "github.com/Nexora48/nexora-api/internal/mail"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// Staff users: invite, change role, disable, reset the authenticator. Only super_admin may manage staff.

const inviteTTL = 72 * time.Hour

var (
	ErrStaffNotFound   = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "staff user not found")
	ErrStaffExists     = httpx.NewError(http.StatusConflict, "EMAIL_TAKEN", "a staff user with this email already exists")
	ErrSelfChange      = httpx.NewError(http.StatusConflict, "CANNOT_MODIFY_SELF", "ask another super admin to change your own role or access")
	ErrLastSuper       = httpx.NewError(http.StatusConflict, "LAST_SUPERADMIN", "there must always be one active super admin")
	ErrInviteInvalid   = httpx.NewError(http.StatusBadRequest, "INVITE_INVALID", "this invitation is invalid or has expired")
	errWeakStaffPasswd = httpx.NewError(http.StatusBadRequest, "WEAK_PASSWORD", "use at least 10 characters with upper case, lower case and a digit")
)

type StaffUser struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	FullName        string     `json:"full_name"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	MFAEnabled      bool       `json:"mfa_enabled"`
	LastLoginAt     *time.Time `json:"last_login_at"`
	InvitePending   bool       `json:"invite_pending"`
	InviteExpiresAt *time.Time `json:"invite_expires_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type InviteResult struct {
	User        *StaffUser `json:"user"`
	InviteToken string     `json:"invite_token"` // always shown once to the inviting super admin, in case the e-mail never arrives
	Emailed     bool       `json:"emailed"`      // an invitation e-mail was handed to the mail server
}

// SetMailer turns on invitation e-mails; consoleURL is where the SuperAdmin web app lives (https://console.example.com).
func (s *Service) SetMailer(m nxmail.Sender, consoleURL string) {
	s.mailer, s.consoleURL = m, strings.TrimRight(consoleURL, "/")
}

func (s *Service) emailInvite(ctx context.Context, u *StaffUser, token string) bool {
	if s.mailer == nil {
		return false
	}
	link := s.consoleURL + "/#/accept-invite?token=" + token
	if s.consoleURL == "" {
		link = "(open the SuperAdmin console and choose \"I have an invitation\", then paste this code) " + token
	}
	body := "Hello " + u.FullName + ",\n\nYou have been invited to the Nexora console as " + u.RoleLabelText() + ".\n\nSet your password here (the link works once and expires in 72 hours):\n" + link +
		"\n\nYou will set up an authenticator app when you first sign in.\n\nIf you were not expecting this, ignore this message.\n"
	if err := s.mailer.Send(ctx, u.Email, "Your Nexora console invitation", body); err != nil {
		slog.Warn("invitation e-mail failed", "err", err)
		return false
	}
	if d, ok := s.mailer.(interface{ Delivers() bool }); ok && !d.Delivers() {
		return false // the console sender only logs
	}
	return true
}

// RoleLabelText names the role for people.
func (u *StaffUser) RoleLabelText() string {
	if u.Role == RoleSuper {
		return "a super admin"
	}
	return "support staff (read-only)"
}

const staffSelect = `SELECT id, email, full_name, role, status, totp_enabled, last_login_at, invite_token_hash IS NOT NULL, invite_expires_at, created_at FROM platform_users`

func (s *Service) scanStaff(row interface{ Scan(...any) error }) (*StaffUser, error) {
	var u StaffUser
	if err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.Status, &u.MFAEnabled, &u.LastLoginAt, &u.InvitePending, &u.InviteExpiresAt, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Service) ListStaff(ctx context.Context) ([]StaffUser, error) {
	rows, err := s.store.pool.Query(ctx, staffSelect+` ORDER BY full_name, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaffUser{}
	for rows.Next() {
		u, err := s.scanStaff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Service) staffByID(ctx context.Context, id uuid.UUID) (*StaffUser, error) {
	u, err := s.scanStaff(s.store.pool.QueryRow(ctx, staffSelect+` WHERE id=$1`, id))
	if err != nil {
		return nil, ErrStaffNotFound
	}
	return u, nil
}

func hashInvite(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

type InviteInput struct {
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

func validRole(r string) bool { return r == RoleSuper || r == RolePlatformSupport() }

func (s *Service) InviteStaff(ctx context.Context, actor uuid.UUID, in InviteInput) (*InviteResult, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.TrimSpace(in.FullName)
	if _, err := mail.ParseAddress(in.Email); err != nil || len(in.Email) > 200 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "a valid email is required")
	}
	if in.FullName == "" || len(in.FullName) > 100 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "full name is required")
	}
	if !validRole(in.Role) {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "role must be super_admin or platform_support")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tok := hex.EncodeToString(raw)
	// the password cannot match anything until the invitation is accepted
	var id uuid.UUID
	err := s.store.pool.QueryRow(ctx, `INSERT INTO platform_users (email, full_name, password_hash, role, invited_by, invite_token_hash, invite_expires_at)
		VALUES ($1,$2,'!invited',$3,$4,$5,$6) RETURNING id`, in.Email, in.FullName, in.Role, actor, hashInvite(tok), time.Now().Add(inviteTTL)).Scan(&id)
	if database.IsUnique(err, "") {
		return nil, ErrStaffExists
	}
	if err != nil {
		return nil, err
	}
	u, err := s.staffByID(ctx, id)
	if err != nil {
		return nil, err
	}
	emailed := s.emailInvite(ctx, u, tok)
	s.platformLog(ctx, actor, nil, "staff.invite", "platform_user", id.String(), map[string]any{"email": in.Email, "role": in.Role, "emailed": emailed})
	return &InviteResult{User: u, InviteToken: tok, Emailed: emailed}, nil
}

// ReinviteStaff issues a fresh token for a user who has not accepted yet.
func (s *Service) ReinviteStaff(ctx context.Context, actor, id uuid.UUID) (*InviteResult, error) {
	u, err := s.staffByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !u.InvitePending {
		return nil, httpx.NewError(http.StatusConflict, "NOT_PENDING", "this user has already accepted the invitation")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tok := hex.EncodeToString(raw)
	if _, err := s.store.pool.Exec(ctx, `UPDATE platform_users SET invite_token_hash=$2, invite_expires_at=$3 WHERE id=$1`, id, hashInvite(tok), time.Now().Add(inviteTTL)); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "staff.reinvite", "platform_user", id.String(), map[string]any{"email": u.Email})
	u, _ = s.staffByID(ctx, id)
	return &InviteResult{User: u, InviteToken: tok, Emailed: s.emailInvite(ctx, u, tok)}, nil
}

// AcceptInvite sets the password (the authenticator is enrolled at the first sign-in). A token works once.
func (s *Service) AcceptInvite(ctx context.Context, token, password string) error {
	if !auth.ValidPasswordPolicy(password) {
		return errWeakStaffPasswd
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.store.pool.Exec(ctx, `UPDATE platform_users SET password_hash=$2, invite_token_hash=NULL, invite_expires_at=NULL
		WHERE invite_token_hash=$1 AND invite_expires_at > now() AND status='active'`, hashInvite(strings.TrimSpace(token)), hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInviteInvalid
	}
	return nil
}

type StaffUpdate struct {
	Role   *string `json:"role"`
	Status *string `json:"status"` // active | disabled
}

func (s *Service) UpdateStaff(ctx context.Context, actor, id uuid.UUID, in StaffUpdate) (*StaffUser, error) {
	if in.Role == nil && in.Status == nil {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "nothing to change")
	}
	if in.Role != nil && !validRole(*in.Role) {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "role must be super_admin or platform_support")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "status must be active or disabled")
	}
	if id == actor {
		return nil, ErrSelfChange
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// lock every active super admin so two changes cannot remove the last one together
	if _, err := tx.Exec(ctx, `SELECT id FROM platform_users WHERE role='super_admin' AND status='active' FOR UPDATE`); err != nil {
		return nil, err
	}
	var role, status string
	if err := tx.QueryRow(ctx, `SELECT role, status FROM platform_users WHERE id=$1 FOR UPDATE`, id).Scan(&role, &status); err != nil {
		return nil, ErrStaffNotFound
	}
	newRole, newStatus := role, status
	if in.Role != nil {
		newRole = *in.Role
	}
	if in.Status != nil {
		newStatus = *in.Status
	}
	if role == "super_admin" && status == "active" && (newRole != "super_admin" || newStatus != "active") {
		var others int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_users WHERE role='super_admin' AND status='active' AND id<>$1`, id).Scan(&others); err != nil {
			return nil, err
		}
		if others == 0 {
			return nil, ErrLastSuper
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_users SET role=$2, status=$3, disabled_at=CASE WHEN $3='disabled' THEN COALESCE(disabled_at, now()) ELSE NULL END WHERE id=$1`, id, newRole, newStatus); err != nil {
		return nil, err
	}
	if newStatus == "disabled" || newRole != role {
		if _, err := tx.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "staff.update", "platform_user", id.String(), map[string]any{
		"before": map[string]any{"role": role, "status": status}, "after": map[string]any{"role": newRole, "status": newStatus}})
	return s.staffByID(ctx, id)
}

// ResetStaffMFA clears the authenticator and recovery codes; the user enrols again at the next sign-in.
func (s *Service) ResetStaffMFA(ctx context.Context, actor, id uuid.UUID) (*StaffUser, error) {
	if id == actor {
		return nil, ErrSelfChange
	}
	if _, err := s.staffByID(ctx, id); err != nil {
		return nil, err
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{
		`UPDATE platform_users SET totp_secret_enc=NULL, totp_enabled=false WHERE id=$1`,
		`DELETE FROM platform_recovery_codes WHERE user_id=$1`,
		`UPDATE platform_refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`,
	} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "staff.reset_mfa", "platform_user", id.String(), nil)
	return s.staffByID(ctx, id)
}
