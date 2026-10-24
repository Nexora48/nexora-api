// Package delivery holds each flat's rules for deliveries and cabs and one-off "expecting a delivery" windows.
package delivery

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

const (
	ModeAsk         = "ask"
	ModeAutoApprove = "auto_approve"
	ModeLeaveAtGate = "leave_at_gate"
)

var (
	ErrNoUnit       = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrBadRule      = httpx.NewError(http.StatusBadRequest, "VALIDATION", "choose delivery or cab, and ask, auto_approve or leave_at_gate")
	ErrCabLeave     = httpx.NewError(http.StatusBadRequest, "VALIDATION", "leave_at_gate only makes sense for deliveries")
	ErrCompanies    = httpx.NewError(http.StatusBadRequest, "VALIDATION", "up to 20 companies, each 2-40 characters")
	ErrCompanyNeed  = httpx.NewError(http.StatusBadRequest, "VALIDATION", "enter the company you are expecting")
	ErrExpectWindow = httpx.NewError(http.StatusBadRequest, "VALIDATION", "hours must be between 1 and 24")
)

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	feats *feature.Service
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, f *feature.Service) *Service {
	return &Service{pool: pool, audit: a, feats: f}
}

var _ visitor.RuleResolver = (*Service)(nil)

// Rule is one scope of a flat's rules.
type Rule struct {
	Scope     string   `json:"scope"`
	Mode      string   `json:"mode"`
	Companies []string `json:"companies"`
}

type Expectation struct {
	ID        uuid.UUID `json:"id"`
	Company   string    `json:"company"`
	ExpiresAt time.Time `json:"expires_at"`
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func matches(list []string, company *string) bool {
	if len(list) == 0 {
		return true
	}
	if company == nil {
		return false
	}
	c := norm(*company)
	for _, x := range list {
		if norm(x) == c {
			return true
		}
	}
	return false
}

// Resolve is called by the gate for delivery and cab arrivals.
func (s *Service) Resolve(ctx context.Context, society, unit uuid.UUID, purpose string, company *string) (visitor.RuleDecision, error) {
	// a society that switched Deliveries off always asks the resident
	if on, err := s.feats.Enabled(ctx, society, "deliveries"); err != nil || !on {
		return visitor.RuleDecision{}, err
	}
	if purpose == "delivery" && company != nil && strings.TrimSpace(*company) != "" {
		var id uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT id FROM delivery_expectations WHERE society_id=$1 AND unit_id=$2 AND used_at IS NULL AND expires_at > now() AND lower(company)=$3 ORDER BY expires_at LIMIT 1`,
			society, unit, norm(*company)).Scan(&id)
		if err == nil {
			return visitor.RuleDecision{Mode: ModeAutoApprove, ExpectationID: &id}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return visitor.RuleDecision{}, err
		}
	}
	var mode string
	var companies []string
	err := s.pool.QueryRow(ctx, `SELECT mode, companies FROM delivery_rules WHERE society_id=$1 AND unit_id=$2 AND scope=$3`, society, unit, purpose).Scan(&mode, &companies)
	if errors.Is(err, pgx.ErrNoRows) {
		return visitor.RuleDecision{}, nil
	}
	if err != nil {
		return visitor.RuleDecision{}, err
	}
	switch mode {
	case ModeAutoApprove:
		if matches(companies, company) {
			return visitor.RuleDecision{Mode: ModeAutoApprove}, nil
		}
	case ModeLeaveAtGate:
		if purpose == "delivery" && matches(companies, company) {
			return visitor.RuleDecision{Mode: ModeLeaveAtGate}, nil
		}
	}
	return visitor.RuleDecision{}, nil
}

// Consume marks a one-off expectation as used (once).
func (s *Service) Consume(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE delivery_expectations SET used_at=now() WHERE id=$1 AND used_at IS NULL`, id)
	return err
}

func (s *Service) unit(ctx context.Context, p authctx.Principal) (uuid.UUID, error) {
	var u *uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	if err != nil || u == nil {
		return uuid.Nil, ErrNoUnit
	}
	return *u, nil
}

// Rules returns both scopes for my flat (defaults to ask) and my open expectations.
func (s *Service) Rules(ctx context.Context, p authctx.Principal) ([]Rule, []Expectation, error) {
	unit, err := s.unit(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	rules := []Rule{{Scope: "delivery", Mode: ModeAsk, Companies: []string{}}, {Scope: "cab", Mode: ModeAsk, Companies: []string{}}}
	rows, err := s.pool.Query(ctx, `SELECT scope, mode, companies FROM delivery_rules WHERE unit_id=$1 AND society_id=$2`, unit, p.SocietyID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var r Rule
		if rows.Scan(&r.Scope, &r.Mode, &r.Companies) == nil {
			for i := range rules {
				if rules[i].Scope == r.Scope {
					if r.Companies == nil {
						r.Companies = []string{}
					}
					rules[i] = r
				}
			}
		}
	}
	rows.Close()
	ex := []Expectation{}
	er, err := s.pool.Query(ctx, `SELECT id, company, expires_at FROM delivery_expectations WHERE unit_id=$1 AND society_id=$2 AND used_at IS NULL AND expires_at > now() ORDER BY expires_at`, unit, p.SocietyID)
	if err != nil {
		return nil, nil, err
	}
	defer er.Close()
	for er.Next() {
		var e Expectation
		if er.Scan(&e.ID, &e.Company, &e.ExpiresAt) == nil {
			ex = append(ex, e)
		}
	}
	return rules, ex, er.Err()
}

func (s *Service) SetRule(ctx context.Context, p authctx.Principal, r Rule) (*Rule, error) {
	if (r.Scope != "delivery" && r.Scope != "cab") || (r.Mode != ModeAsk && r.Mode != ModeAutoApprove && r.Mode != ModeLeaveAtGate) {
		return nil, ErrBadRule
	}
	if r.Scope == "cab" && r.Mode == ModeLeaveAtGate {
		return nil, ErrCabLeave
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, c := range r.Companies {
		c = strings.TrimSpace(c)
		if c == "" || seen[norm(c)] {
			continue
		}
		if l := len([]rune(c)); l < 2 || l > 40 {
			return nil, ErrCompanies
		}
		seen[norm(c)] = true
		clean = append(clean, c)
	}
	if len(clean) > 20 {
		return nil, ErrCompanies
	}
	unit, err := s.unit(ctx, p)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO delivery_rules (society_id, unit_id, scope, mode, companies, updated_by, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (unit_id, scope) DO UPDATE SET mode=EXCLUDED.mode, companies=EXCLUDED.companies, updated_by=EXCLUDED.updated_by, updated_at=now()`,
		p.SocietyID, unit, r.Scope, r.Mode, clean, p.MembershipID); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "delivery.rule", Entity: "unit", EntityID: unit.String(),
		Meta: map[string]any{"scope": r.Scope, "mode": r.Mode, "companies": clean}})
	return &Rule{Scope: r.Scope, Mode: r.Mode, Companies: clean}, nil
}

// Expect lets the next delivery from a company in: valid for the given hours, used once.
func (s *Service) Expect(ctx context.Context, p authctx.Principal, company string, hours int) (*Expectation, error) {
	company = strings.TrimSpace(company)
	if l := len([]rune(company)); l < 2 || l > 40 {
		return nil, ErrCompanyNeed
	}
	if hours == 0 {
		hours = 6
	}
	if hours < 1 || hours > 24 {
		return nil, ErrExpectWindow
	}
	unit, err := s.unit(ctx, p)
	if err != nil {
		return nil, err
	}
	var e Expectation
	err = s.pool.QueryRow(ctx, `INSERT INTO delivery_expectations (society_id, unit_id, company, created_by, expires_at) VALUES ($1,$2,$3,$4, now() + make_interval(hours => $5)) RETURNING id, company, expires_at`,
		p.SocietyID, unit, company, p.MembershipID, hours).Scan(&e.ID, &e.Company, &e.ExpiresAt)
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "delivery.expect", Entity: "unit", EntityID: unit.String(), Meta: map[string]any{"company": company, "hours": hours}})
	return &e, nil
}

// CancelExpectation withdraws one of my open expectations.
func (s *Service) CancelExpectation(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	unit, err := s.unit(ctx, p)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM delivery_expectations WHERE id=$1 AND unit_id=$2 AND society_id=$3 AND used_at IS NULL`, id, unit, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NewError(http.StatusNotFound, "NOT_FOUND", "expectation not found")
	}
	return nil
}
