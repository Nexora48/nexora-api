package superadmin

import (
	"context"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
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
	Commit     string            `json:"commit"`
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

// SetBackupFile points the health page at the file backup.sh touches after every good backup
// (BACKUP_HEARTBEAT_FILE); a ".drill" twin is touched by the restore drill.
func (s *Service) SetBackupFile(path string) { s.backupFile = path }

func buildCommit() string {
	if c := os.Getenv("APP_COMMIT"); c != "" {
		if len(c) > 8 {
			c = c[:8]
		}
		return c
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.revision" && len(kv.Value) >= 8 {
				return kv.Value[:8]
			}
		}
	}
	return ""
}

func fileAge(path string) (time.Duration, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Since(time.Unix(n, 0)), true
}

func (s *Service) Health(ctx context.Context) *Health {
	h := &Health{Version: s.version, Commit: buildCommit(), UptimeSec: int64(time.Since(started).Seconds()), Status: "ok", Checks: []Check{}, Sweepers: heartbeat.All()}
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
	if s.backupFile == "" {
		add("Backups", "warn", "not configured (set BACKUP_HEARTBEAT_FILE and run scripts/backup.sh)")
	} else if age, ok := fileAge(s.backupFile); !ok {
		add("Backups", "fail", "no backup has ever been recorded")
	} else if age > 50*time.Hour {
		add("Backups", "fail", "last good backup "+age.Round(time.Hour).String()+" ago")
	} else if age > 26*time.Hour {
		add("Backups", "warn", "last good backup "+age.Round(time.Hour).String()+" ago")
	} else {
		detail := "last good backup " + age.Round(time.Minute).String() + " ago"
		if drill, ok := fileAge(s.backupFile + ".drill"); ok {
			detail += ", restore drill " + (drill.Round(time.Hour)).String() + " ago"
		} else {
			detail += ", restore drill never run"
		}
		add("Backups", "ok", detail)
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
