package superadmin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

// Platform billing: what Nexora charges each society. Recorded here, collected offline (bank transfer).

type Subscription struct {
	SocietyID        uuid.UUID `json:"society_id"`
	RatePerUnitPaise int64     `json:"rate_per_unit_paise"`
	FlatFeePaise     int64     `json:"flat_fee_paise"`
	BillingDay       int       `json:"billing_day"`
	Status           string    `json:"status"` // active | paused
	StartsOn         string    `json:"starts_on"`
	Units            int       `json:"units"`
	NextAmountPaise  int64     `json:"next_amount_paise"`
}

type PlatformInvoice struct {
	ID          uuid.UUID `json:"id"`
	SocietyID   uuid.UUID `json:"society_id"`
	Society     string    `json:"society"`
	Period      string    `json:"period"`
	Number      string    `json:"number"`
	Units       int       `json:"units"`
	AmountPaise int64     `json:"amount_paise"`
	Status      string    `json:"status"`
	DueDate     string    `json:"due_date"`
	PaidOn      *string   `json:"paid_on,omitempty"`
	Reference   *string   `json:"reference,omitempty"`
	Overdue     bool      `json:"overdue"`
}

var errBadPeriod = httpx.NewError(http.StatusBadRequest, "VALIDATION", "period must be YYYY-MM")

func (s *Service) Subscription(ctx context.Context, society uuid.UUID) (*Subscription, error) {
	var one int
	if err := s.store.pool.QueryRow(ctx, `SELECT 1 FROM societies WHERE id=$1`, society).Scan(&one); err != nil {
		return nil, ErrSocietyNotFound
	}
	sub := &Subscription{SocietyID: society, BillingDay: 1, Status: "paused", StartsOn: time.Now().Format("2006-01-02")}
	var starts time.Time
	err := s.store.pool.QueryRow(ctx, `SELECT rate_per_unit_paise, flat_fee_paise, billing_day, status, starts_on FROM society_subscriptions WHERE society_id=$1`, society).
		Scan(&sub.RatePerUnitPaise, &sub.FlatFeePaise, &sub.BillingDay, &sub.Status, &starts)
	if err == nil {
		sub.StartsOn = starts.Format("2006-01-02")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	_ = s.store.pool.QueryRow(ctx, `SELECT count(*) FROM units WHERE society_id=$1`, society).Scan(&sub.Units)
	sub.NextAmountPaise = sub.RatePerUnitPaise*int64(sub.Units) + sub.FlatFeePaise
	return sub, nil
}

func (s *Service) SetSubscription(ctx context.Context, actor, society uuid.UUID, in Subscription) (*Subscription, error) {
	if in.RatePerUnitPaise < 0 || in.FlatFeePaise < 0 || in.RatePerUnitPaise > 100_000_00 || in.FlatFeePaise > 100_000_000_00 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "amounts are out of range")
	}
	if in.BillingDay < 1 || in.BillingDay > 28 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "the billing day must be 1-28")
	}
	if in.Status != "active" && in.Status != "paused" {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "status must be active or paused")
	}
	start, err := time.Parse("2006-01-02", in.StartsOn)
	if err != nil {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "starts_on must be YYYY-MM-DD")
	}
	if _, err := s.Subscription(ctx, society); err != nil {
		return nil, err
	}
	if _, err := s.store.pool.Exec(ctx, `INSERT INTO society_subscriptions (society_id, rate_per_unit_paise, flat_fee_paise, billing_day, status, starts_on, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (society_id) DO UPDATE SET rate_per_unit_paise=EXCLUDED.rate_per_unit_paise, flat_fee_paise=EXCLUDED.flat_fee_paise, billing_day=EXCLUDED.billing_day, status=EXCLUDED.status, starts_on=EXCLUDED.starts_on, updated_at=now()`,
		society, in.RatePerUnitPaise, in.FlatFeePaise, in.BillingDay, in.Status, start); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, &society, "subscription.set", "subscription", society.String(), map[string]any{"rate_per_unit_paise": in.RatePerUnitPaise, "flat_fee_paise": in.FlatFeePaise, "status": in.Status})
	return s.Subscription(ctx, society)
}

// GenerateInvoices creates the month's invoice for every active subscription that has none yet. Safe to repeat.
func (s *Service) GenerateInvoices(ctx context.Context, actor uuid.UUID, period string) (int, error) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return 0, errBadPeriod
	}
	end := t.AddDate(0, 1, 0)
	rows, err := s.store.pool.Query(ctx, `SELECT sub.society_id, sub.rate_per_unit_paise, sub.flat_fee_paise, sub.billing_day, (SELECT count(*) FROM units u WHERE u.society_id=sub.society_id)
		FROM society_subscriptions sub JOIN societies so ON so.id=sub.society_id WHERE sub.status='active' AND sub.starts_on < $1 AND so.status <> 'deleted'
		AND NOT EXISTS (SELECT 1 FROM platform_invoices pi WHERE pi.society_id=sub.society_id AND pi.period=$2)`, end, period)
	if err != nil {
		return 0, err
	}
	type sub struct {
		id         uuid.UUID
		rate, flat int64
		day, units int
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if err := rows.Scan(&x.id, &x.rate, &x.flat, &x.day, &x.units); err != nil {
			rows.Close()
			return 0, err
		}
		subs = append(subs, x)
	}
	rows.Close()
	made := 0
	for _, x := range subs {
		amount := x.rate*int64(x.units) + x.flat
		if amount <= 0 {
			continue
		}
		due := time.Date(t.Year(), t.Month(), x.day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 14)
		for try := 0; try < 5; try++ {
			var n int
			_ = s.store.pool.QueryRow(ctx, `SELECT count(*) FROM platform_invoices WHERE period=$1`, period).Scan(&n)
			num := fmt.Sprintf("PI-%s%s-%04d", period[2:4], period[5:7], n+1+try)
			_, err := s.store.pool.Exec(ctx, `INSERT INTO platform_invoices (society_id, period, number, units, amount_paise, due_date) VALUES ($1,$2,$3,$4,$5,$6)`, x.id, period, num, x.units, amount, due)
			if err == nil {
				made++
				break
			}
			if database.IsUnique(err, "platform_invoices_society_id_period_key") {
				break
			}
			if !database.IsUnique(err, "platform_invoices_number_key") {
				return made, err
			}
		}
	}
	s.platformLog(ctx, actor, nil, "platform_invoice.generate", "platform_invoice", period, map[string]any{"created": made})
	return made, nil
}

func (s *Service) ListInvoices(ctx context.Context, society *uuid.UUID, status, period string, limit, offset int) ([]PlatformInvoice, int, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT pi.id, pi.society_id, so.name, pi.period, pi.number, pi.units, pi.amount_paise, pi.status, pi.due_date, pi.paid_on, pi.reference, count(*) OVER()
		FROM platform_invoices pi JOIN societies so ON so.id=pi.society_id
		WHERE ($1::uuid IS NULL OR pi.society_id=$1) AND ($2='' OR pi.status=$2) AND ($3='' OR pi.period=$3) ORDER BY pi.period DESC, so.name LIMIT $4 OFFSET $5`, society, status, period, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PlatformInvoice{}
	total := 0
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var v PlatformInvoice
		var due time.Time
		var paid *time.Time
		if err := rows.Scan(&v.ID, &v.SocietyID, &v.Society, &v.Period, &v.Number, &v.Units, &v.AmountPaise, &v.Status, &due, &paid, &v.Reference, &total); err != nil {
			return nil, 0, err
		}
		v.DueDate = due.Format("2006-01-02")
		if paid != nil {
			p := paid.Format("2006-01-02")
			v.PaidOn = &p
		}
		v.Overdue = v.Status == "issued" && due.Before(today)
		out = append(out, v)
	}
	return out, total, rows.Err()
}

func (s *Service) MarkInvoicePaid(ctx context.Context, actor, id uuid.UUID, reference, paidOn string) (*PlatformInvoice, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || len(reference) > 80 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "enter the bank reference (up to 80 characters)")
	}
	day := time.Now().UTC()
	if paidOn != "" {
		d, err := time.Parse("2006-01-02", paidOn)
		if err != nil || d.After(time.Now().UTC().AddDate(0, 0, 1)) {
			return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "paid_on must be a past date, YYYY-MM-DD")
		}
		day = d
	}
	var society uuid.UUID
	err := s.store.pool.QueryRow(ctx, `UPDATE platform_invoices SET status='paid', paid_on=$2, reference=$3 WHERE id=$1 AND status='issued' RETURNING society_id`, id, day, reference).Scan(&society)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NewError(http.StatusConflict, "INVALID_STATE", "this invoice is not open")
	}
	if err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, &society, "platform_invoice.paid", "platform_invoice", id.String(), map[string]any{"reference": reference})
	rows, _, err := s.ListInvoices(ctx, &society, "", "", 100, 0)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (h *Handler) GetSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Subscription(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) PutSubscription(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req Subscription
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.SetSubscription(r.Context(), p.UserID, id, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ListPlatformInvoices(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	var society *uuid.UUID
	if v := q.Get("society_id"); v != "" {
		u, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid society_id"))
			return
		}
		society = &u
	}
	rows, total, err := h.svc.ListInvoices(r.Context(), society, q.Get("status"), q.Get("period"), pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) GeneratePlatformInvoices(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req struct {
		Period string `json:"period"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.GenerateInvoices(r.Context(), p.UserID, req.Period)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int{"created": n}, nil)
}

func (h *Handler) MarkPlatformInvoicePaid(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req struct {
		Reference string `json:"reference"`
		PaidOn    string `json:"paid_on"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.MarkInvoicePaid(r.Context(), p.UserID, id, req.Reference, req.PaidOn)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}
