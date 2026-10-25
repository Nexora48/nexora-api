package superadmin

import (
	"context"
	"strconv"
	"time"

	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
)

// Health is a read-only picture of the platform for the System page. It never returns
// connection strings, keys or other secrets.

var started = time.Now()

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

type Health struct {
	Version    string            `json:"version"`
	UptimeSec  int64             `json:"uptime_seconds"`
	Status     string            `json:"status"`
	Checks     []Check           `json:"checks"`
	Sweepers   []heartbeat.Entry `json:"sweepers"`
	Migrations int               `json:"migration_version"`
}

// SetBuild records the API version shown on the health page.
func (s *Service) SetBuild(version string) { s.version = version }

// SetHub lets the health page show the number of live app connections.
func (s *Service) SetHub(h interface{ Count() int }) { s.hub = h }

func (s *Service) Health(ctx context.Context) *Health {
	h := &Health{Version: s.version, UptimeSec: int64(time.Since(started).Seconds()), Status: "ok", Checks: []Check{}, Sweepers: heartbeat.All()}
	add := func(name, status, detail string) {
		h.Checks = append(h.Checks, Check{name, status, detail})
		if status == "fail" {
			h.Status = "fail"
		} else if status == "warn" && h.Status == "ok" {
			h.Status = "warn"
		}
	}
	pool := s.store.pool
	t0 := time.Now()
	if err := pool.Ping(ctx); err != nil {
		add("PostgreSQL", "fail", "cannot reach the database")
	} else {
		st := pool.Stat()
		var size string
		_ = pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_database_size(current_database()))`).Scan(&size)
		add("PostgreSQL", "ok", "reply in "+time.Since(t0).Round(time.Millisecond).String()+", "+itoaInt(int(st.AcquiredConns()))+" of "+itoaInt(int(st.MaxConns()))+" connections busy, size "+size)
	}
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&h.Migrations, &dirty); err != nil {
		add("Migrations", "warn", "version unknown")
	} else if dirty {
		add("Migrations", "fail", "version "+itoaInt(h.Migrations)+" is dirty")
	} else {
		add("Migrations", "ok", "at version "+itoaInt(h.Migrations))
	}
	if s.rdb == nil {
		add("Redis", "fail", "not configured")
	} else if err := s.rdb.Ping(ctx).Err(); err != nil {
		add("Redis", "fail", "cannot reach Redis")
	} else {
		add("Redis", "ok", "reachable")
	}
	var backlog, failed int
	var oldest *time.Time
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE attempts < 5), count(*) FILTER (WHERE attempts >= 5), min(created_at) FILTER (WHERE attempts < 5)
		FROM notification_outbox WHERE sent_at IS NULL`).Scan(&backlog, &failed, &oldest)
	switch {
	case failed > 0:
		add("Message outbox", "warn", itoaInt(backlog)+" waiting, "+itoaInt(failed)+" failed for good")
	case oldest != nil && time.Since(*oldest) > 15*time.Minute:
		add("Message outbox", "warn", itoaInt(backlog)+" waiting, oldest "+time.Since(*oldest).Round(time.Minute).String())
	default:
		add("Message outbox", "ok", itoaInt(backlog)+" waiting")
	}
	var stuck int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM payment_events WHERE NOT processed AND received_at < now() - interval '5 minutes' AND received_at > now() - interval '24 hours'`).Scan(&stuck)
	if stuck > 0 {
		add("Payment webhooks", "warn", itoaInt(stuck)+" events not processed in the last 24 hours")
	} else {
		add("Payment webhooks", "ok", "no stuck events in the last 24 hours")
	}
	if s.hub != nil {
		add("Live connections", "ok", itoaInt(s.hub.Count())+" apps connected")
	}
	for _, sw := range h.Sweepers {
		if time.Since(sw.At) > 3*time.Hour {
			add("Job "+sw.Name, "warn", "last ran "+time.Since(sw.At).Round(time.Minute).String()+" ago")
		}
	}
	return h
}

func itoaInt(n int) string { return strconv.Itoa(n) }
