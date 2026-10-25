// Package usage rolls the source tables up into one row per society, day and metric
// (usage_daily). A rollup can be repeated for any day and gives the same numbers.
package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
)

// Metric names. Flow metrics describe one day; stock metrics are a snapshot taken at rollup time
// and are only written for today and yesterday (older days cannot be reconstructed).
const (
	ActiveUsers     = "active_users"
	Logins          = "logins"
	Visitors        = "visitors"
	Deliveries      = "deliveries"
	ComplaintsNew   = "complaints_new"
	Notices         = "notices"
	Polls           = "polls"
	SOS             = "sos"
	Bookings        = "bookings"
	InvoicesIssued  = "invoices_issued"
	CollectedPaise  = "collected_paise"
	AttendanceEvent = "attendance_events"
	Notifications   = "notifications"
	Push            = "push"
	SMS             = "sms"
	Email           = "email"

	Units          = "units"
	Residents      = "residents"
	Guards         = "guards"
	StorageBytes   = "storage_bytes"
	ComplaintsOpen = "complaints_open"
)

// Info describes a metric for the screens.
type Info struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Stock bool   `json:"stock"`
	// Service is the feature switch the metric belongs to ("" = platform level).
	Service string `json:"service"`
}

// Catalogue lists every metric in display order.
var Catalogue = []Info{
	{ActiveUsers, "Active users", false, ""},
	{Logins, "Sign-ins", false, ""},
	{Visitors, "Visitors", false, "visitors"},
	{Deliveries, "Deliveries", false, "deliveries"},
	{ComplaintsNew, "New complaints", false, "complaints"},
	{Notices, "Notices published", false, "community"},
	{Polls, "Polls created", false, "polls"},
	{SOS, "SOS alerts", false, "sos"},
	{Bookings, "Amenity bookings", false, "amenities"},
	{InvoicesIssued, "Invoices issued", false, "payments"},
	{CollectedPaise, "Collected (paise)", false, "payments"},
	{AttendanceEvent, "Attendance check-ins", false, "daily_help"},
	{Notifications, "In-app notifications", false, ""},
	{Push, "Push messages", false, ""},
	{SMS, "SMS", false, ""},
	{Email, "Email", false, ""},
	{Units, "Flats", true, ""},
	{Residents, "Residents", true, ""},
	{Guards, "Guards", true, ""},
	{StorageBytes, "Storage (bytes)", true, ""},
	{ComplaintsOpen, "Open complaints", true, "complaints"},
}

// IsMetric reports whether k is a known metric.
func IsMetric(k string) bool {
	for _, m := range Catalogue {
		if m.Key == k {
			return true
		}
	}
	return false
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, now: time.Now} }

// SetClock replaces the clock (tests).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// window gives each society its own day boundaries (its timezone).
const window = `WITH soc AS (
	SELECT id, ($1::date)::timestamp AT TIME ZONE timezone AS d0, (($1::date) + 1)::timestamp AT TIME ZONE timezone AS d1,
	       (($2::timestamptz) AT TIME ZONE timezone)::date AS today
	FROM societies WHERE status <> 'deleted')
`

var flow = map[string]string{
	ActiveUsers: `SELECT s.id, count(DISTINCT t.user_id) FROM soc s JOIN memberships m ON m.society_id=s.id
		JOIN refresh_tokens t ON t.membership_id=m.id AND t.created_at >= s.d0 AND t.created_at < s.d1 GROUP BY s.id`,
	Logins: `SELECT s.id, count(*) FROM soc s JOIN memberships m ON m.society_id=s.id
		JOIN refresh_tokens t ON t.membership_id=m.id AND t.created_at >= s.d0 AND t.created_at < s.d1
		WHERE NOT EXISTS (SELECT 1 FROM refresh_tokens p WHERE p.replaced_by=t.id) GROUP BY s.id`,
	Visitors:        `SELECT s.id, count(*) FROM soc s JOIN visits v ON v.society_id=s.id AND v.created_at >= s.d0 AND v.created_at < s.d1 AND v.purpose <> 'delivery' GROUP BY s.id`,
	Deliveries:      `SELECT s.id, count(*) FROM soc s JOIN visits v ON v.society_id=s.id AND v.created_at >= s.d0 AND v.created_at < s.d1 AND v.purpose = 'delivery' GROUP BY s.id`,
	ComplaintsNew:   `SELECT s.id, count(*) FROM soc s JOIN complaints c ON c.society_id=s.id AND c.created_at >= s.d0 AND c.created_at < s.d1 GROUP BY s.id`,
	Notices:         `SELECT s.id, count(*) FROM soc s JOIN notices n ON n.society_id=s.id AND n.published_at >= s.d0 AND n.published_at < s.d1 GROUP BY s.id`,
	Polls:           `SELECT s.id, count(*) FROM soc s JOIN polls p ON p.society_id=s.id AND p.created_at >= s.d0 AND p.created_at < s.d1 GROUP BY s.id`,
	SOS:             `SELECT s.id, count(*) FROM soc s JOIN sos_events e ON e.society_id=s.id AND e.raised_at >= s.d0 AND e.raised_at < s.d1 GROUP BY s.id`,
	Bookings:        `SELECT s.id, count(*) FROM soc s JOIN amenity_bookings b ON b.society_id=s.id AND b.created_at >= s.d0 AND b.created_at < s.d1 GROUP BY s.id`,
	InvoicesIssued:  `SELECT s.id, count(*) FROM soc s JOIN invoices i ON i.society_id=s.id AND i.issued_at >= s.d0 AND i.issued_at < s.d1 GROUP BY s.id`,
	CollectedPaise:  `SELECT s.id, COALESCE(sum(p.amount_paise),0) FROM soc s JOIN payments p ON p.society_id=s.id AND p.status='ok' AND p.created_at >= s.d0 AND p.created_at < s.d1 GROUP BY s.id`,
	AttendanceEvent: `SELECT s.id, count(*) FROM soc s JOIN attendance_logs a ON a.society_id=s.id AND a.in_at >= s.d0 AND a.in_at < s.d1 GROUP BY s.id`,
	Notifications:   `SELECT s.id, count(*) FROM soc s JOIN notifications n ON n.society_id=s.id AND n.created_at >= s.d0 AND n.created_at < s.d1 GROUP BY s.id`,
}

// outbox rows belong to the society of the user's latest membership, so a user in two societies is counted once.
func outbox(channel string) string {
	return fmt.Sprintf(`SELECT s.id, count(*) FROM notification_outbox o
		JOIN LATERAL (SELECT m.society_id FROM memberships m WHERE m.user_id=o.user_id ORDER BY (m.status='active') DESC, m.id LIMIT 1) ms ON true
		JOIN soc s ON s.id=ms.society_id WHERE o.channel='%s' AND o.created_at >= s.d0 AND o.created_at < s.d1 GROUP BY s.id`, channel)
}

var stock = map[string]string{
	Units:          `SELECT s.id, count(*) FROM soc s JOIN units u ON u.society_id=s.id GROUP BY s.id`,
	Residents:      `SELECT s.id, count(*) FROM soc s JOIN memberships m ON m.society_id=s.id AND m.status IN ('active','suspended') AND m.role_key IN ('resident_owner','resident_tenant','family_member') GROUP BY s.id`,
	Guards:         `SELECT s.id, count(*) FROM soc s JOIN memberships m ON m.society_id=s.id AND m.status IN ('active','suspended') AND m.role_key='guard' GROUP BY s.id`,
	StorageBytes:   `SELECT s.id, COALESCE(sum(a.size_bytes),0) FROM soc s JOIN attachments a ON a.society_id=s.id GROUP BY s.id`,
	ComplaintsOpen: `SELECT s.id, count(*) FROM soc s JOIN complaints c ON c.society_id=s.id AND c.status IN ('open','assigned','in_progress') GROUP BY s.id`,
}

func (s *Service) collect(ctx context.Context, q string, day time.Time, out map[uuid.UUID]int64) error {
	rows, err := s.pool.Query(ctx, window+q, day.Format("2006-01-02"), s.now())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var v int64
		if err := rows.Scan(&id, &v); err != nil {
			return err
		}
		out[id] = v
	}
	return rows.Err()
}

// Rollup recomputes every metric of one calendar day (YYYY-MM-DD in each society's timezone).
// Safe to repeat. Stock metrics are only written when the day is today or yesterday somewhere.
func (s *Service) Rollup(ctx context.Context, day time.Time) error {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := s.pool.Query(ctx, `SELECT id, (($1::timestamptz) AT TIME ZONE timezone)::date FROM societies WHERE status <> 'deleted'`, s.now())
	if err != nil {
		return err
	}
	type soc struct {
		id    uuid.UUID
		today time.Time
	}
	var socs []soc
	for rows.Next() {
		var x soc
		if err := rows.Scan(&x.id, &x.today); err != nil {
			rows.Close()
			return err
		}
		socs = append(socs, x)
	}
	rows.Close()

	batch := &pgx.Batch{}
	put := func(id uuid.UUID, metric string, v int64) {
		batch.Queue(`INSERT INTO usage_daily (society_id, day, metric, value) VALUES ($1,$2,$3,$4)
			ON CONFLICT (society_id, day, metric) DO UPDATE SET value=EXCLUDED.value`, id, day, metric, v)
	}
	run := func(metric, q string, isStock bool) error {
		vals := map[uuid.UUID]int64{}
		if err := s.collect(ctx, q, day, vals); err != nil {
			return fmt.Errorf("usage %s: %w", metric, err)
		}
		for _, so := range socs {
			if isStock {
				d := so.today.Sub(day).Hours() / 24
				if d < 0 || d > 1 {
					continue
				}
			}
			put(so.id, metric, vals[so.id])
		}
		return nil
	}
	for _, m := range Catalogue {
		switch {
		case m.Stock:
			if err := run(m.Key, stock[m.Key], true); err != nil {
				return err
			}
		case m.Key == Push || m.Key == SMS || m.Key == Email:
			if err := run(m.Key, outbox(m.Key), false); err != nil {
				return err
			}
		default:
			if err := run(m.Key, flow[m.Key], false); err != nil {
				return err
			}
		}
	}
	// who was active (kept 30 days for the monthly distinct count)
	if _, err := s.pool.Exec(ctx, window+`INSERT INTO usage_active_users (society_id, day, user_id)
		SELECT DISTINCT s.id, $1::date, t.user_id FROM soc s JOIN memberships m ON m.society_id=s.id
		JOIN refresh_tokens t ON t.membership_id=m.id AND t.created_at >= s.d0 AND t.created_at < s.d1
		ON CONFLICT DO NOTHING`, day.Format("2006-01-02"), s.now()); err != nil {
		return err
	}
	res := s.pool.SendBatch(ctx, batch)
	for i := 0; i < batch.Len(); i++ {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return err
		}
	}
	return res.Close()
}

// Backfill rebuilds the last n days (including today), oldest first.
func (s *Service) Backfill(ctx context.Context, n int) error {
	if n < 1 {
		n = 1
	}
	today := s.now()
	for i := n - 1; i >= 0; i-- {
		if err := s.Rollup(ctx, today.AddDate(0, 0, -i)); err != nil {
			return err
		}
	}
	return nil
}

// Prune drops the per-user activity rows older than 30 days.
func (s *Service) Prune(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM usage_active_users WHERE day < (($1::timestamptz) AT TIME ZONE 'UTC')::date - 30`, s.now())
	return err
}

// Tick is one sweeper pass: today and yesterday are recomputed, old activity rows dropped.
func (s *Service) Tick(ctx context.Context) error {
	now := s.now()
	for _, d := range []time.Time{now.AddDate(0, 0, -1), now} {
		if err := s.Rollup(ctx, d); err != nil {
			return err
		}
	}
	return s.Prune(ctx)
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	_ = s.Tick(ctx)
	heartbeat.Beat("usage-rollup")
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(ctx); err == nil {
				heartbeat.Beat("usage-rollup")
			}
		}
	}
}
