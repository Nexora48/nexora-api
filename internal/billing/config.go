package billing

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
)

type Settings struct {
	DueDay        int    `json:"due_day"`
	GraceDays     int    `json:"grace_days"`
	LateFeeMode   string `json:"late_fee_mode"`  // none | flat | percent
	LateFeeValue  int64  `json:"late_fee_value"` // paise for flat, basis points for percent
	IncludeVacant bool   `json:"include_vacant"`
}

func defaults() Settings { return Settings{DueDay: 10, GraceDays: 5, LateFeeMode: "none"} }

func (s *Service) Settings(ctx context.Context, soc uuid.UUID) (Settings, error) {
	st := defaults()
	err := s.pool.QueryRow(ctx, `SELECT due_day, grace_days, late_fee_mode, late_fee_value, include_vacant FROM billing_settings WHERE society_id=$1`, soc).
		Scan(&st.DueDay, &st.GraceDays, &st.LateFeeMode, &st.LateFeeValue, &st.IncludeVacant)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaults(), nil
	}
	return st, err
}

func (s *Service) SetSettings(ctx context.Context, p authctx.Principal, in Settings) (Settings, error) {
	if in.DueDay < 1 || in.DueDay > 28 {
		return in, invalid("the due day must be 1-28")
	}
	if in.GraceDays < 0 || in.GraceDays > 60 {
		return in, invalid("grace days must be 0-60")
	}
	switch in.LateFeeMode {
	case "none":
		in.LateFeeValue = 0
	case "flat":
		if in.LateFeeValue < 100 || in.LateFeeValue > 100000000 {
			return in, invalid("a flat late fee must be between ₹1 and ₹10,00,000")
		}
	case "percent":
		if in.LateFeeValue < 1 || in.LateFeeValue > 5000 {
			return in, invalid("a percentage late fee must be between 0.01% and 50% a month")
		}
	default:
		return in, invalid("late_fee_mode must be none, flat or percent")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO billing_settings (society_id, due_day, grace_days, late_fee_mode, late_fee_value, include_vacant, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (society_id) DO UPDATE SET due_day=EXCLUDED.due_day, grace_days=EXCLUDED.grace_days, late_fee_mode=EXCLUDED.late_fee_mode,
		late_fee_value=EXCLUDED.late_fee_value, include_vacant=EXCLUDED.include_vacant, updated_at=now()`,
		p.SocietyID, in.DueDay, in.GraceDays, in.LateFeeMode, in.LateFeeValue, in.IncludeVacant)
	if err != nil {
		return in, err
	}
	s.log(ctx, p, "billing.settings", "billing", p.SocietyID, map[string]any{"due_day": in.DueDay, "late_fee_mode": in.LateFeeMode})
	return in, nil
}

// ── charge heads ─────────────────────────────────────────────────────────────

type Rate struct {
	UnitType    string `json:"unit_type"`
	AmountPaise int64  `json:"amount_paise"`
}

type Head struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Calc          string    `json:"calc"` // fixed | per_unit_type | per_sqft
	AmountPaise   int64     `json:"amount_paise"`
	RatePaise     int64     `json:"rate_paise"`
	Recurring     bool      `json:"recurring"`
	OneTimePeriod *string   `json:"one_time_period,omitempty"`
	Active        bool      `json:"active"`
	Rates         []Rate    `json:"rates"`
}

type HeadInput struct {
	Name          string
	Calc          string
	AmountPaise   int64
	RatePaise     int64
	Recurring     bool
	OneTimePeriod string
	Active        bool
	Rates         []Rate
}

func (in *HeadInput) validate() error {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if l := len([]rune(in.Name)); l < 2 || l > 60 {
		return invalid("name must be 2-60 characters")
	}
	switch in.Calc {
	case "fixed":
		if in.AmountPaise <= 0 {
			return invalid("enter the amount per flat")
		}
	case "per_unit_type":
		if in.AmountPaise < 0 || len(in.Rates) == 0 && in.AmountPaise == 0 {
			return invalid("enter an amount for at least one flat type")
		}
		seen := map[string]bool{}
		for _, r := range in.Rates {
			t := strings.TrimSpace(r.UnitType)
			if t == "" || r.AmountPaise < 0 || seen[strings.ToLower(t)] {
				return invalid("flat types must be unique with a non-negative amount")
			}
			seen[strings.ToLower(t)] = true
		}
	case "per_sqft":
		if in.RatePaise <= 0 {
			return invalid("enter the rate per square foot")
		}
	default:
		return invalid("calc must be fixed, per_unit_type or per_sqft")
	}
	if in.AmountPaise > 1_000_000_00 || in.RatePaise > 1_000_000_00 {
		return invalid("amount is out of range")
	}
	if !in.Recurring {
		if err := validPeriod(in.OneTimePeriod); err != nil {
			return invalid("a one-time charge needs the period it applies to (YYYY-MM)")
		}
	} else {
		in.OneTimePeriod = ""
	}
	return nil
}

func (s *Service) Heads(ctx context.Context, soc uuid.UUID, onlyActive bool) ([]*Head, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, calc, amount_paise, rate_paise, recurring, one_time_period, active FROM charge_heads
		WHERE society_id=$1 AND ($2 = false OR active) ORDER BY sort_order, created_at, id`, soc, onlyActive)
	if err != nil {
		return nil, err
	}
	var out []*Head
	by := map[uuid.UUID]*Head{}
	for rows.Next() {
		h := &Head{Rates: []Rate{}}
		if err := rows.Scan(&h.ID, &h.Name, &h.Calc, &h.AmountPaise, &h.RatePaise, &h.Recurring, &h.OneTimePeriod, &h.Active); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
		by[h.ID] = h
	}
	rows.Close()
	if len(out) == 0 {
		return []*Head{}, nil
	}
	ids := make([]uuid.UUID, len(out))
	for i, h := range out {
		ids[i] = h.ID
	}
	rr, err := s.pool.Query(ctx, `SELECT head_id, unit_type, amount_paise FROM charge_head_rates WHERE head_id = ANY($1) ORDER BY unit_type`, ids)
	if err != nil {
		return nil, err
	}
	defer rr.Close()
	for rr.Next() {
		var id uuid.UUID
		var r Rate
		if rr.Scan(&id, &r.UnitType, &r.AmountPaise) == nil {
			by[id].Rates = append(by[id].Rates, r)
		}
	}
	return out, rr.Err()
}

func (s *Service) saveHead(ctx context.Context, p authctx.Principal, id *uuid.UUID, in HeadInput) (*Head, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var period *string
	if !in.Recurring {
		period = &in.OneTimePeriod
	}
	var hid uuid.UUID
	if id == nil {
		err = tx.QueryRow(ctx, `INSERT INTO charge_heads (society_id, name, calc, amount_paise, rate_paise, recurring, one_time_period, active, sort_order)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,(SELECT COALESCE(max(sort_order),0)+1 FROM charge_heads WHERE society_id=$1)) RETURNING id`,
			p.SocietyID, in.Name, in.Calc, in.AmountPaise, in.RatePaise, in.Recurring, period, in.Active).Scan(&hid)
	} else {
		hid = *id
		var tag int64
		t, e := tx.Exec(ctx, `UPDATE charge_heads SET name=$3, calc=$4, amount_paise=$5, rate_paise=$6, recurring=$7, one_time_period=$8, active=$9 WHERE id=$1 AND society_id=$2`,
			hid, p.SocietyID, in.Name, in.Calc, in.AmountPaise, in.RatePaise, in.Recurring, period, in.Active)
		err = e
		if e == nil {
			tag = t.RowsAffected()
			if tag == 0 {
				return nil, ErrNotFound
			}
		}
	}
	if database.IsUnique(err, "charge_heads_name_uq") {
		return nil, ErrHeadNameTaken
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM charge_head_rates WHERE head_id=$1`, hid); err != nil {
		return nil, err
	}
	if in.Calc == "per_unit_type" {
		for _, r := range in.Rates {
			if _, err := tx.Exec(ctx, `INSERT INTO charge_head_rates (head_id, unit_type, amount_paise) VALUES ($1,$2,$3)`, hid, strings.TrimSpace(r.UnitType), r.AmountPaise); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.head(ctx, p.SocietyID, hid)
}

func (s *Service) head(ctx context.Context, soc, id uuid.UUID) (*Head, error) {
	all, err := s.Heads(ctx, soc, false)
	if err != nil {
		return nil, err
	}
	for _, h := range all {
		if h.ID == id {
			return h, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Service) CreateHead(ctx context.Context, p authctx.Principal, in HeadInput) (*Head, error) {
	h, err := s.saveHead(ctx, p, nil, in)
	if err == nil {
		s.log(ctx, p, "billing.head_create", "charge_head", h.ID, map[string]any{"name": h.Name, "calc": h.Calc})
	}
	return h, err
}

func (s *Service) UpdateHead(ctx context.Context, p authctx.Principal, id uuid.UUID, in HeadInput) (*Head, error) {
	h, err := s.saveHead(ctx, p, &id, in)
	if err == nil {
		s.log(ctx, p, "billing.head_update", "charge_head", id, map[string]any{"name": h.Name, "active": h.Active})
	}
	return h, err
}

// DeactivateHead stops a charge from appearing on future invoices (past invoices keep their lines).
func (s *Service) DeactivateHead(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE charge_heads SET active=false WHERE id=$1 AND society_id=$2`, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.log(ctx, p, "billing.head_deactivate", "charge_head", id, nil)
	return nil
}
