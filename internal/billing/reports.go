package billing

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

type DueRow struct {
	UnitID           uuid.UUID `json:"unit_id"`
	Flat             string    `json:"flat"`
	Block            string    `json:"block"`
	OutstandingPaise int64     `json:"outstanding_paise"`
	OverduePaise     int64     `json:"overdue_paise"`
	OldestDue        *string   `json:"oldest_due,omitempty"`
	DaysOverdue      int       `json:"days_overdue"`
	Bucket           string    `json:"bucket"` // current | 1-30 | 31-60 | 61-90 | 90+
	Residents        string    `json:"residents"`
}

type Ageing struct {
	Current int64 `json:"current_paise"`
	D30     int64 `json:"d1_30_paise"`
	D60     int64 `json:"d31_60_paise"`
	D90     int64 `json:"d61_90_paise"`
	D90Plus int64 `json:"d90_plus_paise"`
	Total   int64 `json:"total_paise"`
}

func bucketOf(days int) string {
	switch {
	case days <= 0:
		return "current"
	case days <= 30:
		return "1-30"
	case days <= 60:
		return "31-60"
	case days <= 90:
		return "61-90"
	}
	return "90+"
}

type DuesFilter struct {
	Block, Flat   string
	OverdueOnly   bool
	Limit, Offset int
}

// Dues lists the flats that owe money, worst first, with the society-wide ageing.
func (s *Service) Dues(ctx context.Context, p authctx.Principal, f DuesFilter) ([]DueRow, int, *Ageing, error) {
	today := s.today(ctx, p.SocietyID)
	args := []any{p.SocietyID, today}
	where := ``
	if f.Block != "" {
		args = append(args, f.Block)
		where += fmt.Sprintf(` AND bl.code = $%d`, len(args))
	}
	if q := strings.TrimSpace(f.Flat); q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND (bl.code || '-' || u.number) ILIKE $%d`, len(args))
	}
	having := `sum(i.total_paise-i.paid_paise) > 0`
	if f.OverdueOnly {
		having = `COALESCE(sum(i.total_paise-i.paid_paise) FILTER (WHERE i.due_date < $2),0) > 0`
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, `SELECT u.id, bl.code || '-' || u.number, bl.code,
		sum(i.total_paise-i.paid_paise), COALESCE(sum(i.total_paise-i.paid_paise) FILTER (WHERE i.due_date < $2),0),
		min(i.due_date) FILTER (WHERE i.due_date < $2),
		COALESCE((SELECT string_agg(us.full_name, ', ' ORDER BY m.role_key DESC) FROM memberships m JOIN users us ON us.id=m.user_id WHERE m.unit_id=u.id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant')),''),
		count(*) OVER()
		FROM units u JOIN blocks bl ON bl.id=u.block_id JOIN invoices i ON i.unit_id=u.id AND i.status IN ('issued','partially_paid')
		WHERE u.society_id=$1`+where+` GROUP BY u.id, bl.code, bl.sort_order, u.number HAVING `+having+
		fmt.Sprintf(` ORDER BY 5 DESC, 4 DESC, bl.sort_order, u.number LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	out := []DueRow{}
	total := 0
	for rows.Next() {
		var r DueRow
		var oldest *time.Time
		if err := rows.Scan(&r.UnitID, &r.Flat, &r.Block, &r.OutstandingPaise, &r.OverduePaise, &oldest, &r.Residents, &total); err != nil {
			return nil, 0, nil, err
		}
		if oldest != nil {
			d := oldest.Format("2006-01-02")
			r.OldestDue = &d
			r.DaysOverdue = int(today.Sub(time.Date(oldest.Year(), oldest.Month(), oldest.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
		}
		r.Bucket = bucketOf(r.DaysOverdue)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	ag, err := s.ageing(ctx, p.SocietyID, today)
	return out, total, ag, err
}

func (s *Service) ageing(ctx context.Context, soc uuid.UUID, today time.Time) (*Ageing, error) {
	var a Ageing
	err := s.pool.QueryRow(ctx, `SELECT
		COALESCE(sum(total_paise-paid_paise) FILTER (WHERE $2::date - due_date <= 0),0),
		COALESCE(sum(total_paise-paid_paise) FILTER (WHERE $2::date - due_date BETWEEN 1 AND 30),0),
		COALESCE(sum(total_paise-paid_paise) FILTER (WHERE $2::date - due_date BETWEEN 31 AND 60),0),
		COALESCE(sum(total_paise-paid_paise) FILTER (WHERE $2::date - due_date BETWEEN 61 AND 90),0),
		COALESCE(sum(total_paise-paid_paise) FILTER (WHERE $2::date - due_date > 90),0),
		COALESCE(sum(total_paise-paid_paise),0)
		FROM invoices WHERE society_id=$1 AND status IN ('issued','partially_paid')`, soc, today).Scan(&a.Current, &a.D30, &a.D60, &a.D90, &a.D90Plus, &a.Total)
	return &a, err
}

type PeriodPoint struct {
	Period      string `json:"period"`
	BilledPaise int64  `json:"billed_paise"`
	PaidPaise   int64  `json:"paid_paise"`
}

type BlockRow struct {
	Block       string  `json:"block"`
	BilledPaise int64   `json:"billed_paise"`
	PaidPaise   int64   `json:"paid_paise"`
	Rate        float64 `json:"rate"`
}

type Collection struct {
	Period           string        `json:"period"`
	BilledPaise      int64         `json:"billed_paise"`
	PaidPaise        int64         `json:"paid_paise"` // paid against this period's invoices
	CollectedInMonth int64         `json:"collected_in_month_paise"`
	OutstandingPaise int64         `json:"outstanding_paise"` // all open invoices today
	Rate             float64       `json:"rate"`              // paid / billed, 0-100
	FlatsBilled      int           `json:"flats_billed"`
	FlatsPaid        int           `json:"flats_paid"`
	OverdueFlats     int           `json:"overdue_flats"`
	Series           []PeriodPoint `json:"series"`
	ByBlock          []BlockRow    `json:"by_block"`
	Ageing           *Ageing       `json:"ageing"`
}

func pct(paid, billed int64) float64 {
	if billed <= 0 {
		return 0
	}
	return float64(paid*1000/billed) / 10
}

// Collection is the dashboard of one period.
func (s *Service) Collection(ctx context.Context, p authctx.Principal, period string) (*Collection, error) {
	if period == "" {
		period = s.now().In(s.loc(ctx, p.SocietyID)).Format("2006-01")
	}
	if err := validPeriod(period); err != nil {
		return nil, err
	}
	c := &Collection{Period: period, Series: []PeriodPoint{}, ByBlock: []BlockRow{}}
	live := `status IN ('issued','partially_paid','paid')`
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(total_paise),0), COALESCE(sum(paid_paise),0), count(DISTINCT unit_id), count(DISTINCT unit_id) FILTER (WHERE status='paid')
		FROM invoices WHERE society_id=$1 AND period=$2 AND `+live, p.SocietyID, period).Scan(&c.BilledPaise, &c.PaidPaise, &c.FlatsBilled, &c.FlatsPaid); err != nil {
		return nil, err
	}
	c.Rate = pct(c.PaidPaise, c.BilledPaise)
	loc := s.loc(ctx, p.SocietyID)
	var y, m int
	fmt.Sscanf(period, "%d-%d", &y, &m)
	from := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	_ = loc
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_paise),0) FROM payments WHERE society_id=$1 AND status='ok' AND mode <> 'waiver' AND paid_on >= $2 AND paid_on < $3`,
		p.SocietyID, from, from.AddDate(0, 1, 0)).Scan(&c.CollectedInMonth); err != nil {
		return nil, err
	}
	today := s.today(ctx, p.SocietyID)
	ag, err := s.ageing(ctx, p.SocietyID, today)
	if err != nil {
		return nil, err
	}
	c.Ageing, c.OutstandingPaise = ag, ag.Total
	_ = s.pool.QueryRow(ctx, `SELECT count(DISTINCT unit_id) FROM invoices WHERE society_id=$1 AND status IN ('issued','partially_paid') AND due_date < $2`, p.SocietyID, today).Scan(&c.OverdueFlats)

	for i := 5; i >= 0; i-- {
		d := from.AddDate(0, -i, 0)
		pp := PeriodPoint{Period: d.Format("2006-01")}
		_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(total_paise),0), COALESCE(sum(paid_paise),0) FROM invoices WHERE society_id=$1 AND period=$2 AND `+live, p.SocietyID, pp.Period).Scan(&pp.BilledPaise, &pp.PaidPaise)
		c.Series = append(c.Series, pp)
	}
	rows, err := s.pool.Query(ctx, `SELECT bl.code, COALESCE(sum(i.total_paise),0), COALESCE(sum(i.paid_paise),0) FROM invoices i JOIN units u ON u.id=i.unit_id JOIN blocks bl ON bl.id=u.block_id
		WHERE i.society_id=$1 AND i.period=$2 AND i.status IN ('issued','partially_paid','paid') GROUP BY bl.code, bl.sort_order ORDER BY bl.sort_order, bl.code`, p.SocietyID, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b BlockRow
		if err := rows.Scan(&b.Block, &b.BilledPaise, &b.PaidPaise); err != nil {
			return nil, err
		}
		b.Rate = pct(b.PaidPaise, b.BilledPaise)
		c.ByBlock = append(c.ByBlock, b)
	}
	return c, rows.Err()
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func rs(p int64) string { return fmt.Sprintf("%.2f", float64(p)/100) }

// ExportInvoices writes the invoices of a period as CSV.
func (s *Service) ExportInvoices(ctx context.Context, p authctx.Principal, period string, w io.Writer) (int, error) {
	f := InvoiceFilter{Period: period, Limit: 10000}
	rows, _, err := s.AdminInvoices(ctx, p, f)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Number", "Flat", "Period", "Kind", "Status", "Due date", "Total", "Paid", "Balance"})
	for _, v := range rows {
		n := ""
		if v.Number != nil {
			n = *v.Number
		}
		_ = cw.Write([]string{n, csvSafe(v.Flat), v.Period, v.Kind, v.Status, v.DueDate, rs(v.TotalPaise), rs(v.PaidPaise), rs(v.BalancePaise)})
	}
	cw.Flush()
	s.log(ctx, p, "invoice.export", "invoice", uuid.Nil, map[string]any{"period": period, "rows": len(rows)})
	return len(rows), cw.Error()
}

// ExportDefaulters writes the flats with overdue money as CSV.
func (s *Service) ExportDefaulters(ctx context.Context, p authctx.Principal, w io.Writer) error {
	rows, _, _, err := s.Dues(ctx, p, DuesFilter{OverdueOnly: true, Limit: 10000})
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Flat", "Residents", "Outstanding", "Overdue", "Oldest due", "Days overdue", "Ageing"})
	for _, r := range rows {
		od := ""
		if r.OldestDue != nil {
			od = *r.OldestDue
		}
		_ = cw.Write([]string{csvSafe(r.Flat), csvSafe(r.Residents), rs(r.OutstandingPaise), rs(r.OverduePaise), od, fmt.Sprint(r.DaysOverdue), r.Bucket})
	}
	cw.Flush()
	return cw.Error()
}

// Remind nudges flats with overdue money (all of them, or the chosen ones); each flat at most once per 24 hours.
func (s *Service) Remind(ctx context.Context, p authctx.Principal, units []uuid.UUID) (int, error) {
	rows, _, _, err := s.Dues(ctx, p, DuesFilter{OverdueOnly: true, Limit: 10000})
	if err != nil {
		return 0, err
	}
	want := map[uuid.UUID]bool{}
	for _, u := range units {
		want[u] = true
	}
	sent := 0
	for _, r := range rows {
		if len(want) > 0 && !want[r.UnitID] {
			continue
		}
		var recent bool
		_ = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_logs WHERE society_id=$1 AND action='billing.remind' AND entity_id=$2 AND created_at > now() - interval '24 hours')`, p.SocietyID, r.UnitID.String()).Scan(&recent)
		if recent {
			continue
		}
		s.tellFlat(ctx, p.SocietyID, r.UnitID, "bill.reminder", "Payment reminder", fmt.Sprintf("%s is overdue. Please pay from the Payments page.", money.Rupees(r.OverduePaise)), map[string]any{"type": "bill"}, "normal")
		s.log(ctx, p, "billing.remind", "unit", r.UnitID, map[string]any{"overdue_paise": r.OverduePaise})
		sent++
	}
	return sent, nil
}
