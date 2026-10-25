package superadmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/limits"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// Plans: price, included services and limits. Assigning a plan copies its price into the
// society's subscription and sets the society's services; limits are enforced by internal/limits.

type Plan struct {
	ID               uuid.UUID        `json:"id"`
	Key              string           `json:"key"`
	Name             string           `json:"name"`
	RatePerUnitPaise int64            `json:"rate_per_unit_paise"`
	FlatFeePaise     int64            `json:"flat_fee_paise"`
	Features         []string         `json:"features"`
	Limits           map[string]int64 `json:"limits"`
	Status           string           `json:"status"`
	Societies        int              `json:"societies"`
}

type PlanInput struct {
	Key              string           `json:"key"`
	Name             string           `json:"name"`
	RatePerUnitPaise int64            `json:"rate_per_unit_paise"`
	FlatFeePaise     int64            `json:"flat_fee_paise"`
	Features         []string         `json:"features"`
	Limits           map[string]int64 `json:"limits"`
	ApplyToSocieties bool             `json:"apply_to_societies"` // update: also re-price societies on this plan
}

var (
	ErrPlanNotFound = httpx.NewError(http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
	ErrPlanKeyTaken = httpx.NewError(http.StatusConflict, "PLAN_KEY_TAKEN", "a plan with this key already exists")
	ErrPlanArchived = httpx.NewError(http.StatusConflict, "PLAN_ARCHIVED", "this plan is archived")
	ErrPlanInUse    = httpx.NewError(http.StatusConflict, "PLAN_IN_USE", "move the societies on this plan to another plan first")
	planKeyRe       = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)
)

const planCols = `p.id, p.key, p.name, p.rate_per_unit_paise, p.flat_fee_paise, p.features, p.limits, p.status,
	(SELECT count(*) FROM society_subscriptions s WHERE s.plan_id=p.id)`

func scanPlan(row pgx.Row) (*Plan, error) {
	var p Plan
	var raw []byte
	if err := row.Scan(&p.ID, &p.Key, &p.Name, &p.RatePerUnitPaise, &p.FlatFeePaise, &p.Features, &raw, &p.Status, &p.Societies); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPlanNotFound
		}
		return nil, err
	}
	p.Limits = map[string]int64{}
	var f map[string]float64
	if json.Unmarshal(raw, &f) == nil {
		for k, v := range f {
			p.Limits[k] = int64(v)
		}
	}
	if p.Features == nil {
		p.Features = []string{}
	}
	return &p, nil
}

func (s *Service) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT `+planCols+` FROM plans p ORDER BY (p.status='archived'), p.rate_per_unit_paise, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Service) GetPlan(ctx context.Context, id uuid.UUID) (*Plan, error) {
	return scanPlan(s.store.pool.QueryRow(ctx, `SELECT `+planCols+` FROM plans p WHERE p.id=$1`, id))
}

func (s *Service) validatePlan(ctx context.Context, in *PlanInput, creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Key = strings.ToLower(strings.TrimSpace(in.Key))
	if creating && !planKeyRe.MatchString(in.Key) {
		return httpx.NewError(http.StatusBadRequest, "VALIDATION", "key must be 2-32 letters, digits or underscores")
	}
	if in.Name == "" || len(in.Name) > 60 {
		return httpx.NewError(http.StatusBadRequest, "VALIDATION", "name is required (60 characters at most)")
	}
	if in.RatePerUnitPaise < 0 || in.FlatFeePaise < 0 || in.RatePerUnitPaise > 100_000_00 || in.FlatFeePaise > 100_000_000_00 {
		return httpx.NewError(http.StatusBadRequest, "VALIDATION", "amounts are out of range")
	}
	for k, v := range in.Limits {
		if !limits.Valid(k) {
			return httpx.NewError(http.StatusBadRequest, "VALIDATION", "unknown limit "+k)
		}
		if v < 0 {
			return httpx.NewError(http.StatusBadRequest, "VALIDATION", "limits cannot be negative")
		}
	}
	cat, err := s.Catalogue(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, c := range cat {
		known[c.Key] = true
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, f := range in.Features {
		if !known[f] {
			return httpx.NewError(http.StatusBadRequest, "VALIDATION", "unknown service "+f)
		}
		if !seen[f] {
			seen[f] = true
			clean = append(clean, f)
		}
	}
	sort.Strings(clean)
	in.Features = clean
	if in.Limits == nil {
		in.Limits = map[string]int64{}
	}
	return nil
}

func planMeta(p *Plan) map[string]any {
	return map[string]any{"key": p.Key, "name": p.Name, "rate_per_unit_paise": p.RatePerUnitPaise, "flat_fee_paise": p.FlatFeePaise, "features": p.Features, "limits": p.Limits, "status": p.Status}
}

func (s *Service) CreatePlan(ctx context.Context, actor uuid.UUID, in PlanInput) (*Plan, error) {
	if err := s.validatePlan(ctx, &in, true); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(in.Limits)
	var id uuid.UUID
	err := s.store.pool.QueryRow(ctx, `INSERT INTO plans (key, name, rate_per_unit_paise, flat_fee_paise, features, limits) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		in.Key, in.Name, in.RatePerUnitPaise, in.FlatFeePaise, in.Features, raw).Scan(&id)
	if database.IsUnique(err, "") {
		return nil, ErrPlanKeyTaken
	}
	if err != nil {
		return nil, err
	}
	p, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "plan.create", "plan", id.String(), map[string]any{"after": planMeta(p)})
	return p, nil
}

func (s *Service) UpdatePlan(ctx context.Context, actor, id uuid.UUID, in PlanInput) (*Plan, error) {
	before, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.validatePlan(ctx, &in, false); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(in.Limits)
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE plans SET name=$2, rate_per_unit_paise=$3, flat_fee_paise=$4, features=$5, limits=$6, updated_at=now() WHERE id=$1`,
		id, in.Name, in.RatePerUnitPaise, in.FlatFeePaise, in.Features, raw); err != nil {
		return nil, err
	}
	repriced := int64(0)
	if in.ApplyToSocieties {
		tag, err := tx.Exec(ctx, `UPDATE society_subscriptions SET rate_per_unit_paise=$2, flat_fee_paise=$3, updated_at=now() WHERE plan_id=$1`, id, in.RatePerUnitPaise, in.FlatFeePaise)
		if err != nil {
			return nil, err
		}
		repriced = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	after, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "plan.update", "plan", id.String(), map[string]any{"before": planMeta(before), "after": planMeta(after), "repriced_societies": repriced})
	return after, nil
}

func (s *Service) ArchivePlan(ctx context.Context, actor, id uuid.UUID) (*Plan, error) {
	p, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Societies > 0 {
		return nil, ErrPlanInUse
	}
	if _, err := s.store.pool.Exec(ctx, `UPDATE plans SET status='archived', updated_at=now() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "plan.archive", "plan", id.String(), map[string]any{"key": p.Key})
	return s.GetPlan(ctx, id)
}

type PlanSociety struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	City  string    `json:"city"`
	Units int       `json:"units"`
}

func (s *Service) PlanSocieties(ctx context.Context, id uuid.UUID) ([]PlanSociety, error) {
	if _, err := s.GetPlan(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.store.pool.Query(ctx, `SELECT so.id, so.name, so.city, (SELECT count(*) FROM units u WHERE u.society_id=so.id)
		FROM society_subscriptions sub JOIN societies so ON so.id=sub.society_id WHERE sub.plan_id=$1 AND so.status <> 'deleted' ORDER BY so.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanSociety{}
	for rows.Next() {
		var x PlanSociety
		if err := rows.Scan(&x.ID, &x.Name, &x.City, &x.Units); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type AssignInput struct {
	PlanID          uuid.UUID `json:"plan_id"`
	KeepCustomPrice bool      `json:"keep_custom_price"`
}

type AssignResult struct {
	Plan         *Plan            `json:"plan"`
	Subscription *Subscription    `json:"subscription"`
	Exceeded     []limits.Usage   `json:"exceeded"` // limits the society already passes (nothing is deleted)
	Features     map[string]bool  `json:"features"`
	Changes      []map[string]any `json:"-"`
}

// AssignPlan puts a society on a plan: price copied (unless kept), services set to the plan's list.
func (s *Service) AssignPlan(ctx context.Context, actor, society uuid.UUID, in AssignInput) (*AssignResult, error) {
	p, err := s.GetPlan(ctx, in.PlanID)
	if err != nil {
		return nil, err
	}
	if p.Status != "active" {
		return nil, ErrPlanArchived
	}
	if _, err := s.Subscription(ctx, society); err != nil { // also checks the society exists
		return nil, err
	}
	var oldPlan *string
	_ = s.store.pool.QueryRow(ctx, `SELECT p.key FROM society_subscriptions s JOIN plans p ON p.id=s.plan_id WHERE s.society_id=$1`, society).Scan(&oldPlan)

	if in.KeepCustomPrice {
		_, err = s.store.pool.Exec(ctx, `INSERT INTO society_subscriptions (society_id, rate_per_unit_paise, flat_fee_paise, status, plan_id) VALUES ($1,$2,$3,'active',$4)
			ON CONFLICT (society_id) DO UPDATE SET plan_id=EXCLUDED.plan_id, updated_at=now()`, society, p.RatePerUnitPaise, p.FlatFeePaise, p.ID)
	} else {
		_, err = s.store.pool.Exec(ctx, `INSERT INTO society_subscriptions (society_id, rate_per_unit_paise, flat_fee_paise, status, plan_id) VALUES ($1,$2,$3,'active',$4)
			ON CONFLICT (society_id) DO UPDATE SET plan_id=EXCLUDED.plan_id, rate_per_unit_paise=EXCLUDED.rate_per_unit_paise, flat_fee_paise=EXCLUDED.flat_fee_paise, updated_at=now()`,
			society, p.RatePerUnitPaise, p.FlatFeePaise, p.ID)
	}
	if err != nil {
		return nil, err
	}
	cat, err := s.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	inPlan := map[string]bool{}
	for _, f := range p.Features {
		inPlan[f] = true
	}
	want := map[string]bool{}
	for _, c := range cat {
		want[c.Key] = inPlan[c.Key]
	}
	if _, err := s.SetFeatures(ctx, actor, society, want, "plan "+p.Key); err != nil {
		return nil, err
	}
	snap, err := limits.New(s.store.pool).Snapshot(ctx, society)
	if err != nil {
		return nil, err
	}
	res := &AssignResult{Plan: p, Exceeded: []limits.Usage{}, Features: want}
	for _, u := range snap.Usage {
		if u.Exceeded {
			res.Exceeded = append(res.Exceeded, u)
		}
	}
	res.Subscription, _ = s.Subscription(ctx, society)
	s.platformLog(ctx, actor, &society, "plan.assign", "society", society.String(), map[string]any{
		"from": oldPlan, "to": p.Key, "kept_custom_price": in.KeepCustomPrice, "exceeded": len(res.Exceeded)})
	return res, nil
}
