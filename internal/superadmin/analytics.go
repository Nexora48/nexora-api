package superadmin

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/limits"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/usage"
)

// Analytics read the nightly rollup (usage_daily) plus a few live counts.

// serviceMetrics maps a service switch to the metrics that prove it is being used.
var serviceMetrics = map[string][]string{
	"visitors":         {usage.Visitors},
	"deliveries":       {usage.Deliveries},
	"complaints":       {usage.ComplaintsNew},
	"community":        {usage.Notices},
	"polls":            {usage.Polls},
	"sos":              {usage.SOS},
	"amenities":        {usage.Bookings},
	"payments":         {usage.InvoicesIssued, usage.CollectedPaise},
	"daily_help":       {usage.AttendanceEvent},
	"staff_attendance": {usage.AttendanceEvent},
}

var errRange = httpx.NewError(http.StatusBadRequest, "VALIDATION", "range must be 7, 30, 90 or 365 days")
var errMetric = httpx.NewError(http.StatusBadRequest, "VALIDATION", "unknown metric")

func checkRange(n int) (int, error) {
	switch n {
	case 0:
		return 30, nil
	case 7, 30, 90, 365:
		return n, nil
	}
	return 0, errRange
}

type Point struct {
	Day   string `json:"day"`
	Value int64  `json:"value"`
}

// series returns one point per day for the last n days (today included), zero-filled.
func (s *Service) series(ctx context.Context, metric string, n int, society *uuid.UUID) ([]Point, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT day, sum(value) FROM usage_daily
		WHERE metric=$1 AND day > current_date - $2::int AND ($3::uuid IS NULL OR society_id=$3) GROUP BY day`, metric, n, society)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var d time.Time
		var v int64
		if err := rows.Scan(&d, &v); err != nil {
			return nil, err
		}
		got[d.Format("2006-01-02")] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Point, 0, n)
	today := time.Now()
	for i := n - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		out = append(out, Point{d, got[d]})
	}
	return out, nil
}

func (s *Service) Timeseries(ctx context.Context, metric string, n int, society *uuid.UUID) ([]Point, error) {
	if !usage.IsMetric(metric) {
		return nil, errMetric
	}
	n, err := checkRange(n)
	if err != nil {
		return nil, err
	}
	return s.series(ctx, metric, n, society)
}

type TopRow struct {
	SocietyID uuid.UUID `json:"society_id"`
	Name      string    `json:"name"`
	Value     int64     `json:"value"`
}

func (s *Service) Top(ctx context.Context, metric string, n int) ([]TopRow, error) {
	if !usage.IsMetric(metric) {
		return nil, errMetric
	}
	n, err := checkRange(n)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.pool.Query(ctx, `SELECT so.id, so.name, sum(u.value) FROM usage_daily u JOIN societies so ON so.id=u.society_id
		WHERE u.metric=$1 AND u.day > current_date - $2::int AND so.status <> 'deleted' GROUP BY so.id, so.name HAVING sum(u.value) > 0 ORDER BY 3 DESC, so.name LIMIT 10`, metric, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopRow{}
	for rows.Next() {
		var r TopRow
		if err := rows.Scan(&r.SocietyID, &r.Name, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type ServiceAdoption struct {
	Key        string        `json:"key"`
	Name       string        `json:"name"`
	Enabled    int           `json:"enabled"`
	Used       int           `json:"used"`        // enabled and used in the window
	Unused     []PlanSociety `json:"unused"`      // enabled but not used
	AdoptionPc int           `json:"adoption_pc"` // used / enabled
}

// Adoption: for every service, how many societies have it on and how many really used it recently.
func (s *Service) Adoption(ctx context.Context, n int) ([]ServiceAdoption, error) {
	n, err := checkRange(n)
	if err != nil {
		return nil, err
	}
	cat, err := s.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	out := []ServiceAdoption{}
	for _, c := range cat {
		ms := serviceMetrics[c.Key]
		if ms == nil {
			ms = []string{}
		}
		rows, err := s.store.pool.Query(ctx, `
			SELECT so.id, so.name, so.city, (SELECT count(*) FROM units u WHERE u.society_id=so.id),
			  EXISTS (SELECT 1 FROM usage_daily d WHERE d.society_id=so.id AND d.metric = ANY($3) AND d.day > current_date - $2::int AND d.value > 0)
			FROM societies so LEFT JOIN society_features sf ON sf.society_id=so.id AND sf.feature_key=$1
			WHERE so.status NOT IN ('deleted','suspended') AND COALESCE(sf.enabled, $4) ORDER BY so.name`, c.Key, n, ms, c.DefaultEnabled)
		if err != nil {
			return nil, err
		}
		a := ServiceAdoption{Key: c.Key, Name: c.Name, Unused: []PlanSociety{}}
		for rows.Next() {
			var x PlanSociety
			var used bool
			if err := rows.Scan(&x.ID, &x.Name, &x.City, &x.Units, &used); err != nil {
				rows.Close()
				return nil, err
			}
			a.Enabled++
			if used {
				a.Used++
			} else {
				a.Unused = append(a.Unused, x)
			}
		}
		rows.Close()
		if a.Enabled > 0 {
			a.AdoptionPc = a.Used * 100 / a.Enabled
		}
		out = append(out, a)
	}
	return out, nil
}

type Alert struct {
	Kind      string     `json:"kind"` // limit | outbox | invoice
	Message   string     `json:"message"`
	SocietyID *uuid.UUID `json:"society_id,omitempty"`
}

type Overview struct {
	Societies       int                `json:"societies"`
	ActiveSocieties int                `json:"active_societies"` // had an active user in the window
	DAU             int64              `json:"dau"`              // active users yesterday-or-today, whichever is higher
	MAU             int64              `json:"mau"`              // distinct users in the last 30 days
	MRRPaise        int64              `json:"mrr_paise"`
	Series          map[string][]Point `json:"series"`
	Top             []TopRow           `json:"top"`
	Unused          []ServiceAdoption  `json:"unused"` // services with enabled-but-unused societies, most first
	Alerts          []Alert            `json:"alerts"`
	RangeDays       int                `json:"range_days"`
}

func (s *Service) Overview(ctx context.Context, n int) (*Overview, error) {
	n, err := checkRange(n)
	if err != nil {
		return nil, err
	}
	o := &Overview{RangeDays: n, Series: map[string][]Point{}, Alerts: []Alert{}}
	pool := s.store.pool
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM societies WHERE status <> 'deleted'`).Scan(&o.Societies); err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT society_id) FROM usage_daily WHERE metric='active_users' AND value > 0 AND day > current_date - $1::int`, n).Scan(&o.ActiveSocieties); err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(v),0) FROM (SELECT day, sum(value) v FROM usage_daily WHERE metric='active_users' AND day >= current_date - 1 GROUP BY day) t`).Scan(&o.DAU); err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT user_id) FROM usage_active_users WHERE day > current_date - 30`).Scan(&o.MAU); err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(sub.rate_per_unit_paise * (SELECT count(*) FROM units u WHERE u.society_id=sub.society_id) + sub.flat_fee_paise),0)
		FROM society_subscriptions sub JOIN societies so ON so.id=sub.society_id WHERE sub.status='active' AND so.status <> 'deleted'`).Scan(&o.MRRPaise); err != nil {
		return nil, err
	}
	for _, m := range []string{usage.ActiveUsers, usage.Visitors, usage.Deliveries, usage.ComplaintsNew, usage.Bookings, usage.Notifications} {
		pts, err := s.series(ctx, m, n, nil)
		if err != nil {
			return nil, err
		}
		o.Series[m] = pts
	}
	if o.Top, err = s.Top(ctx, usage.ActiveUsers, n); err != nil {
		return nil, err
	}
	ad, err := s.Adoption(ctx, 30)
	if err != nil {
		return nil, err
	}
	o.Unused = []ServiceAdoption{}
	for _, a := range ad {
		if len(a.Unused) > 0 && a.Enabled > 0 {
			o.Unused = append(o.Unused, a)
		}
	}

	// alerts: limits at 90 %+, a stuck outbox, overdue platform invoices
	rows, err := pool.Query(ctx, `SELECT sub.society_id, so.name FROM society_subscriptions sub JOIN plans p ON p.id=sub.plan_id JOIN societies so ON so.id=sub.society_id
		WHERE p.limits <> '{}'::jsonb AND so.status <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	type ls struct {
		id   uuid.UUID
		name string
	}
	var withLimits []ls
	for rows.Next() {
		var x ls
		if err := rows.Scan(&x.id, &x.name); err != nil {
			rows.Close()
			return nil, err
		}
		withLimits = append(withLimits, x)
	}
	rows.Close()
	lc := limits.New(pool)
	for _, x := range withLimits {
		snap, err := lc.Snapshot(ctx, x.id)
		if err != nil {
			return nil, err
		}
		for _, u := range snap.Usage {
			if u.AtWarning || u.Exceeded {
				id := x.id
				o.Alerts = append(o.Alerts, Alert{Kind: "limit", SocietyID: &id, Message: x.name + ": " + u.Name + " at the plan limit"})
			}
		}
	}
	var stuck int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE sent_at IS NULL AND attempts >= 5`).Scan(&stuck)
	if stuck > 0 {
		o.Alerts = append(o.Alerts, Alert{Kind: "outbox", Message: "Some push/SMS messages failed for good"})
	}
	var overdue int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform_invoices WHERE status='issued' AND due_date < current_date`).Scan(&overdue)
	if overdue > 0 {
		o.Alerts = append(o.Alerts, Alert{Kind: "invoice", Message: "Overdue platform invoices"})
	}
	return o, nil
}

type SocietyUsage struct {
	Plan         *limits.Snapshot   `json:"plan"`
	PlanID       *uuid.UUID         `json:"plan_id"`
	DiffersPlan  []string           `json:"differs_from_plan"` // services whose switch differs from the plan
	MonthTotals  map[string]int64   `json:"month_totals"`
	Series       map[string][]Point `json:"series"`
	Subscription *Subscription      `json:"subscription"`
}

func (s *Service) SocietyUsage(ctx context.Context, id uuid.UUID) (*SocietyUsage, error) {
	sub, err := s.Subscription(ctx, id) // also 404s an unknown society
	if err != nil {
		return nil, err
	}
	snap, err := limits.New(s.store.pool).Snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	u := &SocietyUsage{Plan: snap, Subscription: sub, DiffersPlan: []string{}, MonthTotals: map[string]int64{}, Series: map[string][]Point{}}
	var feats []string
	if err := s.store.pool.QueryRow(ctx, `SELECT p.id, p.features FROM society_subscriptions sub JOIN plans p ON p.id=sub.plan_id WHERE sub.society_id=$1`, id).Scan(&u.PlanID, &feats); err == nil {
		inPlan := map[string]bool{}
		for _, f := range feats {
			inPlan[f] = true
		}
		fr, err := s.societyFeatures(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, f := range fr {
			if f.Enabled != inPlan[f.Key] {
				u.DiffersPlan = append(u.DiffersPlan, f.Key)
			}
		}
	}
	for _, m := range usage.Catalogue {
		if m.Stock {
			continue
		}
		var v int64
		_ = s.store.pool.QueryRow(ctx, `SELECT COALESCE(sum(value),0) FROM usage_daily WHERE society_id=$1 AND metric=$2 AND day >= date_trunc('month', current_date)`, id, m.Key).Scan(&v)
		u.MonthTotals[m.Key] = v
	}
	for _, m := range []string{usage.ActiveUsers, usage.Visitors, usage.ComplaintsNew, usage.Bookings, usage.Notifications} {
		pts, err := s.series(ctx, m, 30, &id)
		if err != nil {
			return nil, err
		}
		u.Series[m] = pts
	}
	return u, nil
}
