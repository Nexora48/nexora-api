package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	issuer            = "nexora"
	audTenant         = "tenant"
	audSelection      = "selection"
	selectionTokenTTL = 5 * time.Minute
)

var (
	ErrTokenExpired = httpx.NewError(http.StatusUnauthorized, "TOKEN_EXPIRED", "access token expired")
	ErrTokenInvalid = httpx.NewError(http.StatusUnauthorized, "TOKEN_INVALID", "invalid token")
)

type accessClaims struct {
	Role         string `json:"role"`
	PWC          bool   `json:"pwc,omitempty"`
	SocietyID    string `json:"sid"`
	MembershipID string `json:"mid"`
	jwt.RegisteredClaims
}

// TokenManager issues and verifies JWTs and generates opaque refresh tokens.
type TokenManager struct {
	secret    []byte
	accessTTL time.Duration
}

func NewTokenManager(secret string, accessTTL time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), accessTTL: accessTTL}
}

func (t *TokenManager) AccessTTL() time.Duration { return t.accessTTL }

func (t *TokenManager) sign(c accessClaims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
}

func (t *TokenManager) IssueAccess(p authctx.Principal) (string, error) {
	now := time.Now()
	return t.sign(accessClaims{
		Role: p.Role, PWC: p.MustChangePassword, SocietyID: p.SocietyID.String(), MembershipID: p.MembershipID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: p.UserID.String(), Audience: jwt.ClaimStrings{audTenant},
			ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(t.accessTTL)),
		},
	})
}

func (t *TokenManager) parse(token, aud string) (*accessClaims, error) {
	var c accessClaims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(aud), jwt.WithIssuer(issuer))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	return &c, nil
}

func (t *TokenManager) ParseAccess(token string) (authctx.Principal, error) {
	c, err := t.parse(token, audTenant)
	if err != nil {
		return authctx.Principal{}, err
	}
	uid, e1 := uuid.Parse(c.Subject)
	sid, e2 := uuid.Parse(c.SocietyID)
	mid, e3 := uuid.Parse(c.MembershipID)
	if e1 != nil || e2 != nil || e3 != nil {
		return authctx.Principal{}, ErrTokenInvalid
	}
	return authctx.Principal{UserID: uid, SocietyID: sid, MembershipID: mid, Role: c.Role, MustChangePassword: c.PWC}, nil
}

// IssueSelection is a short-lived token proving the user passed OTP/password
// but still must choose a society.
func (t *TokenManager) IssueSelection(userID uuid.UUID) (string, error) {
	now := time.Now()
	return t.sign(accessClaims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: userID.String(), Audience: jwt.ClaimStrings{audSelection},
		ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(selectionTokenTTL)),
	}})
}

func (t *TokenManager) ParseSelection(token string) (uuid.UUID, error) {
	c, err := t.parse(token, audSelection)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, ErrTokenInvalid
	}
	return id, nil
}

// NewRefreshToken returns a random opaque token and its SHA-256 hash (stored in DB).
func NewRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
