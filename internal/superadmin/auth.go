package superadmin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	nxmail "github.com/Nexora48/nexora-api/internal/mail"
	"github.com/Nexora48/nexora-api/internal/notify"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	maxFailures = 5
	lockWindow  = 15 * time.Minute
)

var (
	errInvalidCreds = httpx.NewError(http.StatusUnauthorized, "INVALID_CREDENTIALS", "incorrect email or password")
	errLocked       = httpx.NewError(http.StatusTooManyRequests, "ACCOUNT_LOCKED", "too many failed attempts, try again in 15 minutes")
	errMFAInvalid   = httpx.NewError(http.StatusUnauthorized, "MFA_INVALID", "incorrect or already used code")
	errRefreshBad   = httpx.NewError(http.StatusUnauthorized, "TOKEN_INVALID", "invalid refresh token")
	errRefreshExp   = httpx.NewError(http.StatusUnauthorized, "TOKEN_EXPIRED", "session expired, please sign in again")
	errReused       = httpx.NewError(http.StatusUnauthorized, "TOKEN_REUSED", "session revoked for security, please sign in again")
)

type UserDTO struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

type LoginResult struct {
	MFARequired      bool   `json:"mfa_required"`
	MFASetupRequired bool   `json:"mfa_setup_required"`
	MFAToken         string `json:"mfa_token"`
	Secret           string `json:"secret,omitempty"`
	OTPAuthURL       string `json:"otpauth_url,omitempty"`
}

type SessionResult struct {
	AccessToken   string   `json:"access_token"`
	RefreshToken  string   `json:"refresh_token"`
	TokenType     string   `json:"token_type"`
	ExpiresIn     int      `json:"expires_in"`
	User          UserDTO  `json:"user"`
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

type Service struct {
	devFixedMFA string // dev only (see config.DevFixedMFA); empty everywhere else
	store       *store
	tokens      *Tokens
	cipher      *Cipher
	rdb         *redis.Client
	audit       *audit.Logger
	refreshTTL  time.Duration
	onFeatures  func(context.Context, uuid.UUID)
	notifier    *notify.Service
	batch       int
	version     string
	backupFile  string
	mailer      nxmail.Sender
	consoleURL  string
	hub         interface{ Count() int }
}

// SetFeaturesHook is called after service switches change (apps are told to reload them).
func (s *Service) SetFeaturesHook(f func(context.Context, uuid.UUID)) { s.onFeatures = f }

func NewService(pool storePool, tokens *Tokens, cipher *Cipher, rdb *redis.Client, a *audit.Logger, refreshTTL time.Duration) *Service {
	return &Service{store: &store{pool: pool}, tokens: tokens, cipher: cipher, rdb: rdb, audit: a, refreshTTL: refreshTTL}
}

// SetDevFixedMFA makes one 6-digit code valid for every staff 2FA prompt. Dev only: config refuses it elsewhere.
func (s *Service) SetDevFixedMFA(code string) { s.devFixedMFA = code }

func toDTO(u *staff) UserDTO {
	return UserDTO{ID: u.ID.String(), Email: u.Email, FullName: u.FullName, Role: u.Role}
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

func (s *Service) fails(ctx context.Context, key string) int {
	n, _ := s.rdb.Get(ctx, key).Int()
	return n
}

func (s *Service) bump(ctx context.Context, key string) int {
	n, _ := s.rdb.Incr(ctx, key).Result()
	if n == 1 {
		s.rdb.Expire(ctx, key, lockWindow)
	}
	return int(n)
}

func (s *Service) log(ctx context.Context, actor *uuid.UUID, action, ip string, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: actor, ActorType: "platform", Action: action, IP: ip, Meta: meta})
}

// Login is step 1: password. It never issues a session by itself.
func (s *Service) Login(ctx context.Context, email, password, ip string) (*LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	failKey := "platform:login:fail:" + email
	if s.fails(ctx, failKey) >= maxFailures {
		return nil, errLocked
	}
	u, err := s.store.userByEmail(ctx, email)
	valid := false
	if err == nil && u.Status == "active" {
		valid = auth.VerifyPassword(password, u.PasswordHash)
	} else if err != nil && !errors.Is(err, errUserNotFound) {
		return nil, err
	} else {
		auth.VerifyPassword(password, "$argon2id$v=19$m=65536,t=2,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	}
	if !valid {
		n := s.bump(ctx, failKey)
		s.log(ctx, nil, "platform.login_failed", ip, map[string]any{"email": email})
		if n >= maxFailures {
			return nil, errLocked
		}
		return nil, errInvalidCreds
	}
	s.rdb.Del(ctx, failKey)

	mfa, err := s.tokens.IssueMFA(u.ID)
	if err != nil {
		return nil, err
	}
	if u.TOTPEnabled {
		return &LoginResult{MFARequired: true, MFAToken: mfa}, nil
	}
	// first sign-in: enrol an authenticator (a new pending secret each time until confirmed)
	secret, url, err := NewTOTPSecret(u.Email)
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(secret)
	if err != nil {
		return nil, err
	}
	if err := s.store.setTOTP(ctx, u.ID, enc, false); err != nil {
		return nil, err
	}
	return &LoginResult{MFASetupRequired: true, MFAToken: mfa, Secret: secret, OTPAuthURL: url}, nil
}

// VerifyMFA is step 2: a TOTP code (or a recovery code) turns the MFA token into a session.
func (s *Service) VerifyMFA(ctx context.Context, mfaToken, code, recovery, ip, ua string) (*SessionResult, error) {
	userID, err := s.tokens.ParseMFA(mfaToken)
	if err != nil {
		return nil, err
	}
	failKey := "platform:mfa:fail:" + userID.String()
	if s.fails(ctx, failKey) >= maxFailures {
		return nil, errLocked
	}
	u, err := s.store.userByID(ctx, userID)
	if err != nil || u.Status != "active" || u.TOTPSecretEnc == nil {
		return nil, ErrTokenInvalid
	}
	fail := func() (*SessionResult, error) {
		n := s.bump(ctx, failKey)
		s.log(ctx, &u.ID, "platform.mfa_failed", ip, nil)
		if n >= maxFailures {
			return nil, errLocked
		}
		return nil, errMFAInvalid
	}

	var newCodes []string
	switch {
	case strings.TrimSpace(code) != "":
		secret, err := s.cipher.Decrypt(*u.TOTPSecretEnc)
		if err != nil {
			return nil, err
		}
		fixed := s.devFixedMFA != "" && subtle.ConstantTimeCompare([]byte(s.devFixedMFA), []byte(strings.TrimSpace(code))) == 1
		if !fixed {
			step, ok := MatchTOTP(secret, strings.TrimSpace(code), time.Now())
			if !ok {
				return fail()
			}
			// a code is valid once
			fresh, err := s.rdb.SetNX(ctx, fmt.Sprintf("platform:totp:used:%s:%d", u.ID, step), 1, 3*time.Minute).Result()
			if err != nil {
				return nil, err
			}
			if !fresh {
				return fail()
			}
		}
		if !u.TOTPEnabled { // enrolment confirmed
			if err := s.store.setTOTP(ctx, u.ID, *u.TOTPSecretEnc, true); err != nil {
				return nil, err
			}
			newCodes = NewRecoveryCodes(8)
			hashes := make([]string, len(newCodes))
			for i, c := range newCodes {
				hashes[i] = hashCode(c)
			}
			if err := s.store.replaceRecoveryCodes(ctx, u.ID, hashes); err != nil {
				return nil, err
			}
			s.log(ctx, &u.ID, "platform.mfa_enrolled", ip, nil)
		}
	case strings.TrimSpace(recovery) != "" && u.TOTPEnabled:
		ok, err := s.store.useRecoveryCode(ctx, u.ID, hashCode(recovery))
		if err != nil {
			return nil, err
		}
		if !ok {
			return fail()
		}
		s.log(ctx, &u.ID, "platform.recovery_code_used", ip, nil)
	default:
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "enter the 6-digit code")
	}

	s.rdb.Del(ctx, failKey)
	s.store.touchLogin(ctx, u.ID)
	sess, err := s.issueSession(ctx, u, uuid.New(), ip, ua)
	if err != nil {
		return nil, err
	}
	sess.RecoveryCodes = newCodes
	s.log(ctx, &u.ID, "platform.login", ip, nil)
	return sess, nil
}

func (s *Service) issueSession(ctx context.Context, u *staff, family uuid.UUID, ip, ua string) (*SessionResult, error) {
	access, err := s.tokens.IssueAccess(u.ID, u.Role)
	if err != nil {
		return nil, err
	}
	refresh, hash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, err
	}
	if err := s.store.createRefresh(ctx, u.ID, family, hash, time.Now().Add(s.refreshTTL), ip, ua); err != nil {
		return nil, err
	}
	return &SessionResult{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: int(s.tokens.AccessTTL().Seconds()), User: toDTO(u)}, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken, ip, ua string) (*SessionResult, error) {
	newTok, newHash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, err
	}
	userID, err := s.store.rotate(ctx, auth.HashToken(refreshToken), newHash, time.Now().Add(s.refreshTTL), ip, ua)
	switch {
	case errors.Is(err, errRefreshNotFound):
		return nil, errRefreshBad
	case errors.Is(err, errRefreshExpired):
		return nil, errRefreshExp
	case errors.Is(err, errRefreshReused):
		s.log(ctx, &userID, "platform.token_reuse", ip, nil)
		return nil, errReused
	case err != nil:
		return nil, err
	}
	u, err := s.store.userByID(ctx, userID)
	if err != nil || u.Status != "active" {
		_ = s.store.revokeAllForUser(ctx, userID)
		return nil, errRefreshBad
	}
	access, err := s.tokens.IssueAccess(u.ID, u.Role)
	if err != nil {
		return nil, err
	}
	return &SessionResult{AccessToken: access, RefreshToken: newTok, TokenType: "Bearer",
		ExpiresIn: int(s.tokens.AccessTTL().Seconds()), User: toDTO(u)}, nil
}

func (s *Service) Logout(ctx context.Context, p Principal, refreshToken, ip string) error {
	if err := s.store.revokeFamilyByHash(ctx, auth.HashToken(refreshToken), p.UserID); err != nil {
		return err
	}
	s.log(ctx, &p.UserID, "platform.logout", ip, nil)
	return nil
}

func (s *Service) Me(ctx context.Context, p Principal) (*UserDTO, error) {
	u, err := s.store.userByID(ctx, p.UserID)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	d := toDTO(u)
	return &d, nil
}
