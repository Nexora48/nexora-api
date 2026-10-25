// Package billing holds the society's money: what is charged (heads), invoices with a draft/issue step, the ledger,
// payments and their allocation, receipts, dues and reports. Everything is integer paise.
package billing

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

var periodRe = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

func unprocessable(code, msg string) error {
	return httpx.NewError(http.StatusUnprocessableEntity, code, msg)
}
func conflict(code, msg string) error { return httpx.NewError(http.StatusConflict, code, msg) }
func invalid(msg string) error        { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

var (
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "not found")
	ErrNoUnit        = conflict("NO_UNIT", "your account is not linked to a flat")
	ErrInvalidState  = conflict("INVALID_STATE", "this cannot be changed in its current state")
	ErrHeadNameTaken = conflict("NAME_TAKEN", "a charge with this name already exists")
	ErrNothingDue    = conflict("NOTHING_DUE", "there is nothing to pay")
)

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	note  *notify.Service
	hub   *realtime.Hub
	now   func() time.Time
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, note *notify.Service, hub *realtime.Hub) *Service {
	return &Service{pool: pool, audit: a, note: note, hub: hub, now: time.Now}
}

// SetClock lets tests move time (late fees, reminders, overdue).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

func (s *Service) log(ctx context.Context, p authctx.Principal, action, entity string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: entity, EntityID: id.String(), Meta: meta})
}

func (s *Service) loc(ctx context.Context, soc uuid.UUID) *time.Location {
	var tz string
	_ = s.pool.QueryRow(ctx, `SELECT timezone FROM societies WHERE id=$1`, soc).Scan(&tz)
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+1800)
}

// today is the society's current local date at midnight UTC-agnostic (a date value).
func (s *Service) today(ctx context.Context, soc uuid.UUID) time.Time {
	n := s.now().In(s.loc(ctx, soc))
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) (uuid.UUID, error) {
	var u *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	if u == nil {
		return uuid.Nil, ErrNoUnit
	}
	return *u, nil
}

// flatLabel returns "A-101" style labels for the given units.
func (s *Service) flatLabel(ctx context.Context, unit uuid.UUID) string {
	var l string
	_ = s.pool.QueryRow(ctx, `SELECT b.code || '-' || u.number FROM units u JOIN blocks b ON b.id=u.block_id WHERE u.id=$1`, unit).Scan(&l)
	return l
}

func validPeriod(p string) error {
	if !periodRe.MatchString(p) {
		return invalid("period must be YYYY-MM")
	}
	return nil
}

// ym turns "2026-11" into "2611" for numbers.
func ym(period string) string { return period[2:4] + period[5:7] }

// nextNumber returns the next number of a sequence (INV/RCP) for a society and year-month, inside the caller's transaction.
func nextNumber(ctx context.Context, tx pgx.Tx, table string, soc uuid.UUID, yymm, prefix string) (string, error) {
	var n int
	err := tx.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %s (society_id, ym, last) VALUES ($1,$2,1)
		ON CONFLICT (society_id, ym) DO UPDATE SET last = %s.last + 1 RETURNING last`, table, table), soc, yymm).Scan(&n)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%04d", prefix, yymm, n), nil
}

// publish tells the flat and the office to refresh (ids and kinds only).
func (s *Service) publish(ctx context.Context, soc, unit uuid.UUID, typ string, id uuid.UUID) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(ctx, realtime.Event{Type: typ, SocietyID: soc, Unit: &unit, Roles: []string{"society_admin", "committee"}, Data: map[string]any{"id": id}})
}

func (s *Service) tellFlat(ctx context.Context, soc, unit uuid.UUID, typ, title, body string, data map[string]any, prio string) {
	if s.note == nil {
		return
	}
	users := s.note.UnitResidents(ctx, soc, unit)
	if len(users) == 0 {
		return
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: typ, Title: title, Body: body, Data: data, Priority: prio})
}
