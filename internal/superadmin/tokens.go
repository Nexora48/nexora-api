package superadmin

import (
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	platformIssuer = "nexora-platform"
	audPlatform    = "platform"
	audMFA         = "platform-mfa"
	mfaTokenTTL    = 5 * time.Minute
)

var (
	ErrTokenExpired = httpx.NewError(http.StatusUnauthorized, "TOKEN_EXPIRED", "session expired")
	ErrTokenInvalid = httpx.NewError(http.StatusUnauthorized, "TOKEN_INVALID", "invalid token")
	ErrMFAExpired   = httpx.NewError(http.StatusUnauthorized, "MFA_EXPIRED", "sign-in expired, start again")
)

type claims struct {
	Role string `json:"role,omitempty"`
	jwt.RegisteredClaims
}

// Tokens signs platform JWTs with a secret that is different from the tenant secret and uses
// its own audiences, so tenant and platform tokens can never be swapped.
type Tokens struct {
	secret    []byte
	accessTTL time.Duration
}

func NewTokens(secret string, accessTTL time.Duration) *Tokens {
	return &Tokens{secret: []byte(secret), accessTTL: accessTTL}
}

func (t *Tokens) AccessTTL() time.Duration { return t.accessTTL }

func (t *Tokens) sign(c claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
}

func (t *Tokens) parse(token, aud string) (*claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(aud), jwt.WithIssuer(platformIssuer))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	return &c, nil
}

func (t *Tokens) IssueAccess(userID uuid.UUID, role string) (string, error) {
	now := time.Now()
	return t.sign(claims{Role: role, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: platformIssuer, Subject: userID.String(), Audience: jwt.ClaimStrings{audPlatform}, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(t.accessTTL)),
	}})
}

func (t *Tokens) ParseAccess(token string) (Principal, error) {
	c, err := t.parse(token, audPlatform)
	if err != nil {
		return Principal{}, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil || c.Role == "" {
		return Principal{}, ErrTokenInvalid
	}
	return Principal{UserID: id, Role: c.Role}, nil
}

// IssueMFA proves the password step passed; it cannot be used as an access token.
func (t *Tokens) IssueMFA(userID uuid.UUID) (string, error) {
	now := time.Now()
	return t.sign(claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: platformIssuer, Subject: userID.String(), Audience: jwt.ClaimStrings{audMFA}, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(mfaTokenTTL)),
	}})
}

func (t *Tokens) ParseMFA(token string) (uuid.UUID, error) {
	c, err := t.parse(token, audMFA)
	if err != nil {
		if errors.Is(err, ErrTokenExpired) {
			return uuid.Nil, ErrMFAExpired
		}
		return uuid.Nil, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, ErrTokenInvalid
	}
	return id, nil
}
