// Package gatepass is the resident's own gate pass: a short-lived signed QR the resident shows on their phone,
// and the guard-side check that turns a scan into an entry or exit in the resident gate log.
package gatepass

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	qrPrefix = "NXR1:"
	// TTL is how long one QR keeps working. The app asks for a fresh one before it runs out, so a forwarded
	// screenshot is useless within minutes.
	TTL = 5 * time.Minute
	// duplicateWindow: a second scan of the same person this soon is the guard scanning twice, not a real exit.
	duplicateWindow = 8 * time.Second
	// insideWindow: an "in" older than this is treated as a missed exit, so the next scan is an entry.
	insideWindow = 24 * time.Hour
)

var (
	ErrNoFlat  = httpx.NewError(http.StatusConflict, "NO_FLAT", "a gate pass is for people who live in a flat")
	ErrBadPass = httpx.NewError(http.StatusBadRequest, "BAD_PASS", "scan a resident gate pass QR")
)

func notValid(reason string) *httpx.AppError {
	return httpx.NewError(http.StatusConflict, "PASS_NOT_VALID", "pass is not valid: "+reason)
}

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	key   []byte
	nowFn func() time.Time
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, secret string) *Service {
	k := hmac.New(sha256.New, []byte(secret))
	k.Write([]byte("resident-qr"))
	return &Service{pool: pool, audit: a, key: k.Sum(nil), nowFn: time.Now}
}

func (s *Service) sign(id uuid.UUID, exp int64) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id.String() + "." + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// qr is <prefix><membership id>.<expiry unix>.<signature>.
func (s *Service) qr(id uuid.UUID, exp time.Time) string {
	return qrPrefix + id.String() + "." + strconv.FormatInt(exp.Unix(), 10) + "." + s.sign(id, exp.Unix())
}

// parse checks the shape and the signature. The expiry is returned for the caller to compare with the clock.
func (s *Service) parse(payload string) (id uuid.UUID, exp time.Time, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(payload), qrPrefix)
	if !found {
		return uuid.Nil, time.Time{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return uuid.Nil, time.Time{}, false
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, time.Time{}, false
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return uuid.Nil, time.Time{}, false
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.sign(id, unix))) {
		return uuid.Nil, time.Time{}, false
	}
	return id, time.Unix(unix, 0), true
}

// Pass is what the resident app shows.
type Pass struct {
	QR         string    `json:"qr"`
	ExpiresAt  time.Time `json:"expires_at"`
	Name       string    `json:"name"`
	UnitNumber string    `json:"unit_number"`
	BlockCode  string    `json:"block_code"`
	Role       string    `json:"role"`
}

func roleLabel(key string) string {
	switch key {
	case "resident_owner":
		return "owner"
	case "resident_tenant":
		return "tenant"
	case "family_member":
		return "family"
	}
	return key
}

// Mine issues a fresh QR for the caller's own membership.
func (s *Service) Mine(ctx context.Context, p authctx.Principal) (*Pass, error) {
	var pass Pass
	var unit, block *string
	var role string
	err := s.pool.QueryRow(ctx, `SELECT u.full_name, un.number, b.code, m.role_key
		FROM memberships m JOIN users u ON u.id=m.user_id
		LEFT JOIN units un ON un.id=m.unit_id LEFT JOIN blocks b ON b.id=un.block_id
		WHERE m.id=$1 AND m.user_id=$2 AND m.society_id=$3 AND m.status='active'`, p.MembershipID, p.UserID, p.SocietyID).
		Scan(&pass.Name, &unit, &block, &role)
	if err != nil {
		return nil, ErrNoFlat
	}
	if unit == nil {
		return nil, ErrNoFlat
	}
	pass.UnitNumber, pass.Role = *unit, roleLabel(role)
	if block != nil {
		pass.BlockCode = *block
	}
	pass.ExpiresAt = s.nowFn().Add(TTL)
	pass.QR = s.qr(p.MembershipID, pass.ExpiresAt)
	return &pass, nil
}

// Resident is what a guard may see about a scanned pass: no phone number.
type Resident struct {
	MembershipID uuid.UUID `json:"membership_id"`
	Name         string    `json:"name"`
	UnitNumber   string    `json:"unit_number"`
	BlockCode    string    `json:"block_code"`
	Role         string    `json:"role"`
}

// Result answers a scan. Reasons: INVALID (not one of ours or tampered), EXPIRED, NOT_FOUND, SUSPENDED.
type Result struct {
	Valid     bool       `json:"valid"`
	Reason    string     `json:"reason,omitempty"`
	Resident  *Resident  `json:"resident,omitempty"`
	Inside    bool       `json:"inside"`             // the last logged move was an entry
	Next      string     `json:"next,omitempty"`     // what recording this scan would log: in or out
	Recorded  bool       `json:"recorded,omitempty"` // true once a log row was written
	Duplicate bool       `json:"duplicate,omitempty"`
	At        *time.Time `json:"at,omitempty"`
}

func (s *Service) check(ctx context.Context, societyID uuid.UUID, qr string) (*Result, uuid.UUID, *uuid.UUID, error) {
	id, exp, ok := s.parse(qr)
	if !ok {
		return &Result{Reason: "INVALID"}, uuid.Nil, nil, nil
	}
	if !exp.After(s.nowFn()) {
		return &Result{Reason: "EXPIRED"}, uuid.Nil, nil, nil
	}
	var r Resident
	var status string
	var unitID *uuid.UUID
	var unit, block *string
	var role string
	err := s.pool.QueryRow(ctx, `SELECT m.id, u.full_name, m.unit_id, un.number, b.code, m.role_key, m.status
		FROM memberships m JOIN users u ON u.id=m.user_id
		LEFT JOIN units un ON un.id=m.unit_id LEFT JOIN blocks b ON b.id=un.block_id
		WHERE m.id=$1 AND m.society_id=$2`, id, societyID).Scan(&r.MembershipID, &r.Name, &unitID, &unit, &block, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return &Result{Reason: "NOT_FOUND"}, uuid.Nil, nil, nil
	}
	if err != nil {
		return nil, uuid.Nil, nil, err
	}
	r.Role = roleLabel(role)
	if unit != nil {
		r.UnitNumber = *unit
	}
	if block != nil {
		r.BlockCode = *block
	}
	if status != "active" {
		return &Result{Reason: "SUSPENDED", Resident: &r}, uuid.Nil, nil, nil
	}
	res := &Result{Valid: true, Resident: &r, Next: "in"}
	var dir string
	var at time.Time
	err = s.pool.QueryRow(ctx, `SELECT direction, created_at FROM resident_gate_logs
		WHERE society_id=$1 AND membership_id=$2 AND created_at > now() - make_interval(secs => $3)
		ORDER BY created_at DESC LIMIT 1`, societyID, id, insideWindow.Seconds()).Scan(&dir, &at)
	if err == nil {
		res.At = &at
		if dir == "in" {
			res.Inside, res.Next = true, "out"
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, uuid.Nil, nil, err
	}
	return res, id, unitID, nil
}

// Verify shows the guard who this is and what scanning would log. Nothing is written.
func (s *Service) Verify(ctx context.Context, p authctx.Principal, qr string) (*Result, error) {
	res, _, _, err := s.check(ctx, p.SocietyID, qr)
	return res, err
}

// Record logs the entry or exit for a valid pass. A second scan within a few seconds changes nothing.
func (s *Service) Record(ctx context.Context, p authctx.Principal, qr string) (*Result, error) {
	res, id, unitID, err := s.check(ctx, p.SocietyID, qr)
	if err != nil {
		return nil, err
	}
	if !res.Valid {
		return nil, notValid(res.Reason)
	}
	if res.At != nil && s.nowFn().Sub(*res.At) < duplicateWindow {
		res.Duplicate = true
		return res, nil
	}
	var at time.Time
	if err := s.pool.QueryRow(ctx, `INSERT INTO resident_gate_logs (society_id, membership_id, unit_id, direction, recorded_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING created_at`, p.SocietyID, id, unitID, res.Next, p.UserID).Scan(&at); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "resident.gate." + res.Next, Entity: "membership", EntityID: id.String()})
	res.Recorded, res.At = true, &at
	res.Inside = res.Next == "in"
	if res.Inside {
		res.Next = "out"
	} else {
		res.Next = "in"
	}
	return res, nil
}
