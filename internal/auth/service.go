package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/society"
	"github.com/Nexora48/nexora-api/internal/user"
)

const (
	maxLoginFailures = 5
	lockDuration     = 15 * time.Minute
)

// Roles allowed to sign in with email+password (the admin app).
var adminRoles = map[string]bool{"society_admin": true, "committee": true}

var (
	ErrPhoneNotRegistered = httpx.NewError(http.StatusNotFound, "PHONE_NOT_REGISTERED", "this number is not registered with any society")
	ErrInvalidCreds       = httpx.NewError(http.StatusUnauthorized, "INVALID_CREDENTIALS", "incorrect email or password")
	ErrAccountLocked      = httpx.NewError(http.StatusTooManyRequests, "ACCOUNT_LOCKED", "too many failed attempts, try again in 15 minutes")
	ErrNotAuthorisedApp   = httpx.NewError(http.StatusForbidden, "NOT_AUTHORISED", "this account cannot sign in here")
	ErrSocietySuspended   = httpx.NewError(http.StatusForbidden, "SOCIETY_SUSPENDED", "Your society's access is paused. Please contact Nexora support.")
	ErrRefreshInvalid     = httpx.NewError(http.StatusUnauthorized, "TOKEN_INVALID", "invalid refresh token")
	ErrRefreshExpired     = httpx.NewError(http.StatusUnauthorized, "TOKEN_EXPIRED", "session expired, please sign in again")
	ErrTokenReused        = httpx.NewError(http.StatusUnauthorized, "TOKEN_REUSED", "session revoked for security, please sign in again")
	ErrWeakPassword       = httpx.NewError(http.StatusBadRequest, "WEAK_PASSWORD", "use at least 8 characters with a letter and a number")
	ErrSamePassword       = httpx.NewError(http.StatusBadRequest, "WEAK_PASSWORD", "choose a password different from the current one")
	ErrBadMembership      = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "membership not available")
)

type Service struct {
	repo       *Repository
	users      *user.Repository
	societies  *society.Repository
	tokens     *TokenManager
	otp        *OTPService
	rdb        *redis.Client
	audit      *audit.Logger
	refreshTTL time.Duration
}

func NewService(repo *Repository, users *user.Repository, societies *society.Repository, tokens *TokenManager,
	otp *OTPService, rdb *redis.Client, a *audit.Logger, refreshTTL time.Duration) *Service {
	return &Service{repo: repo, users: users, societies: societies, tokens: tokens, otp: otp, rdb: rdb, audit: a, refreshTTL: refreshTTL}
}

// ClientInfo is request metadata stored with sessions and audit entries.
type ClientInfo struct {
	IP, UserAgent string
	Device        *DeviceInfo
}

// ── OTP flow ─────────────────────────────────────────────────────────────────

func (s *Service) RequestOTP(ctx context.Context, phone string) error {
	if !ValidPhone(phone) {
		return ErrInvalidPhone
	}
	u, err := s.users.FindByPhone(ctx, phone)
	if errors.Is(err, user.ErrNotFound) {
		return ErrPhoneNotRegistered
	}
	if err != nil {
		return err
	}
	ms, err := s.users.ListActiveMemberships(ctx, u.ID)
	if err != nil {
		return err
	}
	if u.Status != "active" || len(ms) == 0 {
		return ErrPhoneNotRegistered
	}
	return s.otp.Request(ctx, phone)
}

func (s *Service) VerifyOTP(ctx context.Context, phone, code string, ci ClientInfo) (*LoginResponse, error) {
	if err := s.otp.Verify(ctx, phone, code); err != nil {
		s.audit.Log(ctx, audit.Entry{Action: "auth.otp_failed", Meta: map[string]any{"phone": phone}, IP: ci.IP})
		return nil, err
	}
	u, err := s.users.FindByPhone(ctx, phone)
	if errors.Is(err, user.ErrNotFound) || (err == nil && u.Status != "active") {
		return nil, ErrPhoneNotRegistered
	}
	if err != nil {
		return nil, err
	}
	ms, err := s.users.ListActiveMemberships(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return s.finishLogin(ctx, u, ms, ci, "otp")
}

// ── Password flow (admin app) ────────────────────────────────────────────────

func (s *Service) PasswordLogin(ctx context.Context, email, password string, ci ClientInfo) (*LoginResponse, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	failKey := "login:fail:" + email
	if n, _ := s.rdb.Get(ctx, failKey).Int(); n >= maxLoginFailures {
		return nil, ErrAccountLocked
	}
	u, err := s.users.FindByEmail(ctx, email)
	valid := false
	if err == nil && u.PasswordHash != nil && u.Status == "active" {
		valid = VerifyPassword(password, *u.PasswordHash)
	} else if err != nil && !errors.Is(err, user.ErrNotFound) {
		return nil, err
	} else {
		// Equalise timing for unknown users.
		VerifyPassword(password, "$argon2id$v=19$m=65536,t=2,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	}
	if !valid {
		n, _ := s.rdb.Incr(ctx, failKey).Result()
		if n == 1 {
			s.rdb.Expire(ctx, failKey, lockDuration)
		}
		s.audit.Log(ctx, audit.Entry{Action: "auth.login_failed", Meta: map[string]any{"email": email}, IP: ci.IP})
		if n >= maxLoginFailures {
			return nil, ErrAccountLocked
		}
		return nil, ErrInvalidCreds
	}
	s.rdb.Del(ctx, failKey)

	all, err := s.users.ListActiveMemberships(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	var ms []user.Membership
	for _, m := range all {
		if adminRoles[m.RoleKey] {
			ms = append(ms, m)
		}
	}
	if len(ms) == 0 {
		return nil, ErrNotAuthorisedApp
	}
	return s.finishLogin(ctx, u, ms, ci, "password")
}

// ── Shared ───────────────────────────────────────────────────────────────────

func (s *Service) finishLogin(ctx context.Context, u *user.User, ms []user.Membership, ci ClientInfo, method string) (*LoginResponse, error) {
	var usable []user.Membership
	for _, m := range ms {
		if m.SocietyStatus != society.StatusSuspended {
			usable = append(usable, m)
		}
	}
	if len(ms) > 0 && len(usable) == 0 {
		return nil, ErrSocietySuspended
	}
	if len(usable) == 0 {
		return nil, ErrNotAuthorisedApp
	}
	_ = s.users.TouchLogin(ctx, u.ID)
	if len(usable) == 1 {
		return s.issueSession(ctx, u, usable[0], uuid.New(), ci, method)
	}
	sel, err := s.tokens.IssueSelection(u.ID)
	if err != nil {
		return nil, err
	}
	resp := &LoginResponse{SelectionRequired: true, SelectionToken: sel}
	for _, m := range usable {
		resp.Memberships = append(resp.Memberships, toMembershipDTO(m))
	}
	return resp, nil
}

func (s *Service) SelectSociety(ctx context.Context, selectionToken string, membershipID uuid.UUID, ci ClientInfo) (*LoginResponse, error) {
	userID, err := s.tokens.ParseSelection(selectionToken)
	if err != nil {
		return nil, err
	}
	m, err := s.users.GetMembership(ctx, membershipID)
	if errors.Is(err, user.ErrNotFound) || (err == nil && (m.UserID != userID || m.Status != "active" || !m.AppAccess)) {
		return nil, ErrBadMembership
	}
	if err != nil {
		return nil, err
	}
	if m.SocietyStatus == society.StatusSuspended {
		return nil, ErrSocietySuspended
	}
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return nil, ErrBadMembership
	}
	return s.issueSession(ctx, u, *m, uuid.New(), ci, "select")
}

func (s *Service) issueSession(ctx context.Context, u *user.User, m user.Membership, family uuid.UUID, ci ClientInfo, method string) (*LoginResponse, error) {
	userID := u.ID
	access, err := s.tokens.IssueAccess(authctx.Principal{UserID: userID, SocietyID: m.SocietyID, MembershipID: m.ID, Role: m.RoleKey, MustChangePassword: u.MustChangePassword})
	if err != nil {
		return nil, err
	}
	refresh, hash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}
	deviceID := ""
	if ci.Device != nil {
		deviceID = ci.Device.DeviceID
		_ = s.users.UpsertDevice(ctx, userID, ci.Device.DeviceID, ci.Device.Platform, ci.Device.FCMToken, ci.Device.AppVersion)
	}
	if err := s.repo.CreateRefresh(ctx, userID, m.ID, family, hash, time.Now().Add(s.refreshTTL), deviceID, ci.IP, ci.UserAgent); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &userID, SocietyID: &m.SocietyID, Action: "auth.login",
		Meta: map[string]any{"method": method, "role": m.RoleKey}, IP: ci.IP})
	dto := toMembershipDTO(m)
	return &LoginResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: int(s.tokens.AccessTTL().Seconds()), Membership: &dto, MustChangePassword: u.MustChangePassword}, nil
}

// Refresh rotates the refresh token and issues a new access token.
func (s *Service) Refresh(ctx context.Context, refreshToken string, ci ClientInfo) (*LoginResponse, error) {
	newTok, newHash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}
	info, err := s.repo.Rotate(ctx, HashToken(refreshToken), newHash, time.Now().Add(s.refreshTTL), ci.IP, ci.UserAgent)
	switch {
	case errors.Is(err, errRefreshNotFound):
		return nil, ErrRefreshInvalid
	case errors.Is(err, errRefreshExpired):
		return nil, ErrRefreshExpired
	case errors.Is(err, errRefreshReused):
		s.audit.Log(ctx, audit.Entry{ActorID: &info.UserID, Action: "auth.token_reuse", IP: ci.IP})
		return nil, ErrTokenReused
	case err != nil:
		return nil, err
	}
	m, err := s.users.GetMembership(ctx, info.MembershipID)
	if err != nil || m.Status != "active" || !m.AppAccess || m.SocietyStatus == society.StatusSuspended {
		_ = s.repo.RevokeFamily(ctx, info.FamilyID)
		if err == nil && m.SocietyStatus == society.StatusSuspended {
			return nil, ErrSocietySuspended
		}
		return nil, ErrRefreshInvalid
	}
	u, uerr := s.users.FindByID(ctx, info.UserID)
	if uerr != nil {
		return nil, ErrRefreshInvalid
	}
	access, err := s.tokens.IssueAccess(authctx.Principal{UserID: info.UserID, SocietyID: m.SocietyID, MembershipID: m.ID, Role: m.RoleKey, MustChangePassword: u.MustChangePassword})
	if err != nil {
		return nil, err
	}
	dto := toMembershipDTO(*m)
	return &LoginResponse{AccessToken: access, RefreshToken: newTok, TokenType: "Bearer",
		ExpiresIn: int(s.tokens.AccessTTL().Seconds()), Membership: &dto, MustChangePassword: u.MustChangePassword}, nil
}

func (s *Service) Logout(ctx context.Context, p authctx.Principal, refreshToken, ip string) error {
	if err := s.repo.RevokeFamilyByHash(ctx, HashToken(refreshToken), p.UserID); err != nil {
		return err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "auth.logout", IP: ip})
	return nil
}

// ChangePassword sets a new password, revokes every session of the user and returns a fresh session
// for the same membership (this is also how the temporary-password gate is lifted).
func (s *Service) ChangePassword(ctx context.Context, p authctx.Principal, current, next string, ci ClientInfo) (*LoginResponse, error) {
	u, err := s.users.FindByID(ctx, p.UserID)
	if err != nil || u.PasswordHash == nil {
		return nil, ErrInvalidCreds
	}
	if !VerifyPassword(current, *u.PasswordHash) {
		s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "auth.change_password_failed", IP: ci.IP})
		return nil, ErrInvalidCreds
	}
	if !ValidPasswordPolicy(next) {
		return nil, ErrWeakPassword
	}
	if next == current {
		return nil, ErrSamePassword
	}
	hash, err := HashPassword(next)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetPassword(ctx, u.ID, hash); err != nil {
		return nil, err
	}
	if err := s.repo.RevokeAllForUser(ctx, u.ID); err != nil {
		return nil, err
	}
	m, err := s.users.GetMembership(ctx, p.MembershipID)
	if err != nil || m.UserID != u.ID || m.Status != "active" || !m.AppAccess {
		return nil, ErrBadMembership
	}
	u.MustChangePassword = false
	s.audit.Log(ctx, audit.Entry{ActorID: &u.ID, SocietyID: &p.SocietyID, Action: "auth.change_password", IP: ci.IP})
	return s.issueSession(ctx, u, *m, uuid.New(), ci, "change_password")
}
