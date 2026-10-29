// Package dashboard answers the office dashboard in one call: today's numbers, a 7-day visitor chart,
// complaints by status, this month's collections and the latest visitors and complaints.
// A section is null when its service switch is off for the society; empty data is an empty section, never invented.
package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/billing"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Service struct {
	pool  *pgxpool.Pool
	feats *feature.Service
	bill  *billing.Service
}

func New(pool *pgxpool.Pool, feats *feature.Service, bill *billing.Service) *Service {
	return &Service{pool: pool, feats: feats, bill: bill}
}

type Residents struct {
	Total        int `json:"total"`
	YetToSignIn  int `json:"yet_to_sign_in"`
	Flats        int `json:"flats"`
	OccupiedFlat int `json:"occupied_flats"`
}

type DayPoint struct {
	Day         string `json:"day"` // YYYY-MM-DD in the society's time zone
	Label       string `json:"label"`
	PreApproved int    `json:"pre_approved"`
	WalkIn      int    `json:"walk_in"`
}

type Visitors struct {
	Today           int             `json:"today"`
	InsideNow       int             `json:"inside_now"`
	AwaitingNow     int             `json:"awaiting_approval"`
	ApprovedToday   int             `json:"approved_today"`
	DeliveriesToday int             `json:"deliveries_today"`
	AvgGateMinutes  *float64        `json:"avg_gate_minutes"` // null when nobody left today
	Series          []DayPoint      `json:"series"`
	Recent          []RecentVisitor `json:"recent"`
}

type RecentVisitor struct {
	Name   string    `json:"name"`
	Flat   string    `json:"flat"`
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

type Complaints struct {
	Open          int               `json:"open"`
	Assigned      int               `json:"assigned"`
	InProgress    int               `json:"in_progress"`
	ResolvedToday int               `json:"resolved_today"`
	Pending       int               `json:"pending"` // open + assigned + in progress
	Overdue       int               `json:"overdue"`
	Recent        []RecentComplaint `json:"recent"`
}

type RecentComplaint struct {
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	Flat       string    `json:"flat"`
	ReportedBy string    `json:"reported_by"`
	Assignee   *string   `json:"assignee"`
	At         time.Time `json:"at"`
}

type Payments struct {
	Period           string  `json:"period"`
	BilledPaise      int64   `json:"billed_paise"`
	PaidPaise        int64   `json:"paid_paise"`
	CollectedMonth   int64   `json:"collected_in_month_paise"`
	CollectedToday   int64   `json:"collected_today_paise"`
	OutstandingPaise int64   `json:"outstanding_paise"`
	OverdueFlats     int     `json:"overdue_flats"`
	Rate             float64 `json:"rate"`
	HasInvoices      bool    `json:"has_invoices"`
}

type Summary struct {
	Today      string      `json:"today"`
	Residents  Residents   `json:"residents"`
	Visitors   *Visitors   `json:"visitors"`
	Complaints *Complaints `json:"complaints"`
	Payments   *Payments   `json:"payments"`
}

const flatLabel = `bl.code || '-' || un.number`

// Summary builds the dashboard for the signed-in society.
func (s *Service) Summary(ctx context.Context, p authctx.Principal) (*Summary, error) {
	feats, err := s.feats.Map(ctx, p.SocietyID)
	if err != nil {
		return nil, err
	}
	var tz string
	if err := s.pool.QueryRow(ctx, `SELECT timezone FROM societies WHERE id=$1`, p.SocietyID).Scan(&tz); err != nil {
		return nil, err
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	out := &Summary{Today: now.Format("2006-01-02")}

	// residents and flats
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM memberships m WHERE m.society_id=$1 AND m.status IN ('active','suspended') AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		       (SELECT count(*) FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.society_id=$1 AND m.status='active' AND u.last_login_at IS NULL AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		       (SELECT count(*) FROM units WHERE society_id=$1),
		       (SELECT count(DISTINCT m.unit_id) FROM memberships m WHERE m.society_id=$1 AND m.status IN ('active','suspended') AND m.role_key IN ('resident_owner','resident_tenant'))`, p.SocietyID).
		Scan(&out.Residents.Total, &out.Residents.YetToSignIn, &out.Residents.Flats, &out.Residents.OccupiedFlat); err != nil {
		return nil, err
	}

	if feats["visitors"] {
		v, err := s.visitors(ctx, p.SocietyID, tz, now)
		if err != nil {
			return nil, err
		}
		out.Visitors = v
	}
	if feats["complaints"] {
		c, err := s.complaints(ctx, p.SocietyID, tz)
		if err != nil {
			return nil, err
		}
		out.Complaints = c
	}
	if feats["payments"] {
		pm, err := s.payments(ctx, p, tz)
		if err != nil {
			return nil, err
		}
		out.Payments = pm
	}
	return out, nil
}

// dayStart is the start of today in the society's time zone.
const dayStart = `(date_trunc('day', now() AT TIME ZONE $2) AT TIME ZONE $2)`

func (s *Service) visitors(ctx context.Context, soc uuid.UUID, tz string, now time.Time) (*Visitors, error) {
	v := &Visitors{Series: []DayPoint{}, Recent: []RecentVisitor{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status <> 'cancelled' AND (created_at >= `+dayStart+` OR entered_at >= `+dayStart+`)),
		       count(*) FILTER (WHERE status = 'inside'),
		       count(*) FILTER (WHERE status = 'waiting'),
		       count(*) FILTER (WHERE decided_at >= `+dayStart+` AND status IN ('approved','inside','completed')),
		       count(*) FILTER (WHERE purpose = 'delivery' AND status <> 'cancelled' AND (created_at >= `+dayStart+` OR entered_at >= `+dayStart+`)),
		       avg(extract(epoch FROM exited_at - entered_at) / 60) FILTER (WHERE exited_at >= `+dayStart+` AND entered_at IS NOT NULL)
		FROM visits WHERE society_id=$1`, soc, tz).Scan(&v.Today, &v.InsideNow, &v.AwaitingNow, &v.ApprovedToday, &v.DeliveriesToday, &v.AvgGateMinutes); err != nil {
		return nil, err
	}
	// the last 7 days, one point per day even when nothing happened
	rows, err := s.pool.Query(ctx, `
		WITH days AS (SELECT generate_series((now() AT TIME ZONE $2)::date - 6, (now() AT TIME ZONE $2)::date, interval '1 day')::date AS d)
		SELECT d, count(v.id) FILTER (WHERE v.source='pre_approved'), count(v.id) FILTER (WHERE v.source='walk_in')
		FROM days LEFT JOIN visits v ON v.society_id=$1 AND v.entered_at >= (d::timestamp AT TIME ZONE $2) AND v.entered_at < ((d + 1)::timestamp AT TIME ZONE $2)
		GROUP BY d ORDER BY d`, soc, tz)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d time.Time
		var pt DayPoint
		if err := rows.Scan(&d, &pt.PreApproved, &pt.WalkIn); err != nil {
			rows.Close()
			return nil, err
		}
		pt.Day, pt.Label = d.Format("2006-01-02"), d.Format("Mon")
		v.Series = append(v.Series, pt)
	}
	rows.Close()
	rv, err := s.pool.Query(ctx, `
		SELECT v.visitor_name, `+flatLabel+`, v.status, COALESCE(v.entered_at, v.created_at)
		FROM visits v JOIN units un ON un.id=v.unit_id JOIN blocks bl ON bl.id=un.block_id
		WHERE v.society_id=$1 AND v.status NOT IN ('cancelled','invited','expired') ORDER BY COALESCE(v.entered_at, v.created_at) DESC LIMIT 5`, soc)
	if err != nil {
		return nil, err
	}
	defer rv.Close()
	for rv.Next() {
		var r RecentVisitor
		if err := rv.Scan(&r.Name, &r.Flat, &r.Status, &r.At); err != nil {
			return nil, err
		}
		v.Recent = append(v.Recent, r)
	}
	return v, rv.Err()
}

func (s *Service) complaints(ctx context.Context, soc uuid.UUID, tz string) (*Complaints, error) {
	c := &Complaints{Recent: []RecentComplaint{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status='open'), count(*) FILTER (WHERE status='assigned'), count(*) FILTER (WHERE status='in_progress'),
		       count(*) FILTER (WHERE status IN ('resolved','closed') AND resolved_at >= `+dayStart+`),
		       count(*) FILTER (WHERE status IN ('open','assigned','in_progress') AND target_date IS NOT NULL AND target_date < (now() AT TIME ZONE $2)::date)
		FROM complaints WHERE society_id=$1`, soc, tz).Scan(&c.Open, &c.Assigned, &c.InProgress, &c.ResolvedToday, &c.Overdue); err != nil {
		return nil, err
	}
	c.Pending = c.Open + c.Assigned + c.InProgress
	rows, err := s.pool.Query(ctx, `
		SELECT c.title, c.status, `+flatLabel+`, ru.full_name, COALESCE(au.full_name, NULLIF(c.assigned_vendor,'')), c.created_at
		FROM complaints c JOIN units un ON un.id=c.unit_id JOIN blocks bl ON bl.id=un.block_id
		JOIN memberships rm ON rm.id=c.raised_by_membership_id JOIN users ru ON ru.id=rm.user_id
		LEFT JOIN memberships am ON am.id=c.assigned_membership_id LEFT JOIN users au ON au.id=am.user_id
		WHERE c.society_id=$1 ORDER BY c.created_at DESC LIMIT 5`, soc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r RecentComplaint
		if err := rows.Scan(&r.Title, &r.Status, &r.Flat, &r.ReportedBy, &r.Assignee, &r.At); err != nil {
			return nil, err
		}
		c.Recent = append(c.Recent, r)
	}
	return c, rows.Err()
}

func (s *Service) payments(ctx context.Context, p authctx.Principal, tz string) (*Payments, error) {
	col, err := s.bill.Collection(ctx, p, "")
	if err != nil {
		return nil, err
	}
	pm := &Payments{Period: col.Period, BilledPaise: col.BilledPaise, PaidPaise: col.PaidPaise, CollectedMonth: col.CollectedInMonth,
		OutstandingPaise: col.OutstandingPaise, OverdueFlats: col.OverdueFlats, Rate: col.Rate, HasInvoices: col.FlatsBilled > 0}
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_paise),0) FROM payments WHERE society_id=$1 AND status='ok' AND mode <> 'waiver' AND created_at >= `+dayStart, p.SocietyID, tz).Scan(&pm.CollectedToday); err != nil {
		return nil, err
	}
	return pm, nil
}

// Handler serves GET /society/dashboard.
func (s *Service) Handler(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.From(r.Context())
	if !ok {
		httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "sign in first"))
		return
	}
	v, err := s.Summary(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}
