package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// MaxPINTries is how many wrong PINs a device gets before its PIN is removed and its sessions end.
const MaxPINTries = 5

var (
	ErrPINFormat = httpx.NewError(http.StatusBadRequest, "PIN_WEAK", "use 4 to 6 digits that are not all the same or in a row (like 1234)")
	ErrPINDevice = httpx.NewError(http.StatusBadRequest, "VALIDATION", "device_id is required")
	ErrPINNone   = httpx.NewError(http.StatusNotFound, "PIN_NOT_SET", "no PIN is set on this phone; sign in with OTP")
	ErrPINLocked = httpx.NewError(http.StatusLocked, "PIN_LOCKED", "too many wrong PINs; sign in again with an OTP")
)

// PinService stores and checks the app PIN. The PIN is hashed with argon2id after being bound to the user and device with a
// server secret, so a copy of the table alone cannot be tried offline.
type PinService struct {
	pool   *pgxpool.Pool
	secret []byte
	audit  *audit.Logger
}

func NewPinService(pool *pgxpool.Pool, secret []byte, a *audit.Logger) *PinService {
	return &PinService{pool: pool, secret: secret, audit: a}
}

func (s *PinService) bind(p authctx.Principal, device, pin string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(p.UserID.String() + "|" + device + "|" + pin))
	return hex.EncodeToString(m.Sum(nil))
}

// ValidPIN accepts 4 to 6 digits that are not one repeated digit and not a straight run.
func ValidPIN(pin string) bool {
	if len(pin) < 4 || len(pin) > 6 {
		return false
	}
	same, up, down := true, true, true
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
		if i > 0 {
			same = same && pin[i] == pin[0]
			up = up && pin[i] == pin[i-1]+1
			down = down && pin[i] == pin[i-1]-1
		}
	}
	return !same && !up && !down
}

// Status says whether this device has a PIN.
func (s *PinService) Status(ctx context.Context, p authctx.Principal, device string) (bool, error) {
	if strings.TrimSpace(device) == "" {
		return false, ErrPINDevice
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_pins WHERE user_id=$1 AND device_id=$2`, p.UserID, device).Scan(&n)
	return n > 0, err
}

// Set creates or replaces the PIN of this device and clears its wrong-try counter.
func (s *PinService) Set(ctx context.Context, p authctx.Principal, device, pin string) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return ErrPINDevice
	}
	if !ValidPIN(pin) {
		return ErrPINFormat
	}
	hash, err := HashPassword(s.bind(p, device, pin))
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_pins (user_id, device_id, pin_hash) VALUES ($1,$2,$3)
		ON CONFLICT (user_id, device_id) DO UPDATE SET pin_hash=$3, failed=0, updated_at=now()`, p.UserID, device, hash); err != nil {
		return err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "auth.pin_set", Entity: "user", EntityID: p.UserID.String()})
	return nil
}

// Verify checks the PIN. A wrong PIN answers 401 PIN_INVALID and says how many tries are left; the last wrong try removes the PIN and
// signs this device out (PIN_LOCKED), so the person has to prove the number again with an OTP.
func (s *PinService) Verify(ctx context.Context, p authctx.Principal, device, pin string) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return ErrPINDevice
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var hash string
	var failed int
	err = tx.QueryRow(ctx, `SELECT pin_hash, failed FROM user_pins WHERE user_id=$1 AND device_id=$2 FOR UPDATE`, p.UserID, device).Scan(&hash, &failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPINNone
	}
	if err != nil {
		return err
	}
	if VerifyPassword(s.bind(p, device, pin), hash) {
		if failed > 0 {
			if _, err := tx.Exec(ctx, `UPDATE user_pins SET failed=0 WHERE user_id=$1 AND device_id=$2`, p.UserID, device); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	failed++
	if failed >= MaxPINTries {
		if _, err := tx.Exec(ctx, `DELETE FROM user_pins WHERE user_id=$1 AND device_id=$2`, p.UserID, device); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND device_id=$2 AND revoked_at IS NULL`, p.UserID, device); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "auth.pin_locked", Entity: "user", EntityID: p.UserID.String()})
		return ErrPINLocked
	}
	if _, err := tx.Exec(ctx, `UPDATE user_pins SET failed=$3 WHERE user_id=$1 AND device_id=$2`, p.UserID, device, failed); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	left := MaxPINTries - failed
	msg := fmt.Sprintf("Wrong PIN. %d tries left.", left)
	if left == 1 {
		msg = "Wrong PIN. 1 try left."
	}
	return httpx.NewError(http.StatusUnauthorized, "PIN_INVALID", msg)
}

// Forget removes the PIN of this device (sign-out or "forgot PIN").
func (s *PinService) Forget(ctx context.Context, p authctx.Principal, device string) error {
	if strings.TrimSpace(device) == "" {
		return ErrPINDevice
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM user_pins WHERE user_id=$1 AND device_id=$2`, p.UserID, device)
	return err
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type PinHandler struct{ svc *PinService }

func NewPinHandler(svc *PinService) *PinHandler { return &PinHandler{svc: svc} }

func pinPrincipal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	p, ok := authctx.From(r.Context())
	if !ok {
		httpx.Fail(w, ErrTokenInvalid)
	}
	return p, ok
}

type pinBody struct {
	DeviceID string `json:"device_id"`
	PIN      string `json:"pin"`
}

// GET /me/pin?device_id=
func (h *PinHandler) Status(w http.ResponseWriter, r *http.Request) {
	p, ok := pinPrincipal(w, r)
	if !ok {
		return
	}
	has, err := h.svc.Status(r.Context(), p, r.URL.Query().Get("device_id"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"has_pin": has}, nil)
}

// PUT /me/pin {device_id, pin}
func (h *PinHandler) Set(w http.ResponseWriter, r *http.Request) {
	p, ok := pinPrincipal(w, r)
	if !ok {
		return
	}
	var b pinBody
	if err := httpx.Decode(w, r, &b); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Set(r.Context(), p, b.DeviceID, b.PIN); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

// POST /me/pin/verify {device_id, pin}
func (h *PinHandler) Verify(w http.ResponseWriter, r *http.Request) {
	p, ok := pinPrincipal(w, r)
	if !ok {
		return
	}
	var b pinBody
	if err := httpx.Decode(w, r, &b); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Verify(r.Context(), p, b.DeviceID, b.PIN); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

// DELETE /me/pin?device_id=
func (h *PinHandler) Forget(w http.ResponseWriter, r *http.Request) {
	p, ok := pinPrincipal(w, r)
	if !ok {
		return
	}
	if err := h.svc.Forget(r.Context(), p, r.URL.Query().Get("device_id")); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
