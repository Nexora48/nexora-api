// Command loadtest boots the API in-process (embedded Postgres, in-memory Redis), seeds one synthetic society
// and drives it with virtual users. It prints latency percentiles per call and a few single-shot timings
// (invoice generation, reports, SOS fan-out), and can write the result as a Markdown file for docs/perf.
//
//	go run ./cmd/loadtest                       # 200 virtual users, 30 s, 500 flats
//	go run ./cmd/loadtest -vus 100 -dur 15s -flats 300 -out docs/perf/run.md
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/db"
	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/testutil"
)

var (
	vus        = flag.Int("vus", 200, "virtual users (about 70% residents, 10% guards, 20% office)")
	dur        = flag.Duration("dur", 30*time.Second, "how long the virtual users run")
	flats      = flag.Int("flats", 500, "flats in the synthetic society")
	visits     = flag.Int("visits", 20000, "historical visits to seed")
	notes      = flag.Int("notifications", 50000, "historical notifications to seed")
	out        = flag.String("out", "", "write a Markdown report to this file")
	reqTimeout = flag.Duration("timeout", 60*time.Second, "per-request client timeout")
	conns      = flag.Int("conns", 25, "database pool size")
	probeFlag  = flag.Bool("probe", false, "run each request kind once against a one-connection pool to find nested connection use")
	rls        = flag.Bool("rls", false, "run the API through the row-level-security role")
	p95Limit   = flag.Duration("p95", 300*time.Millisecond, "target p95 for normal reads")
)

type sample struct {
	name string
	d    time.Duration
	code int
	err  bool
}

type recorder struct {
	mu sync.Mutex
	s  []sample
}

func (r *recorder) add(name string, d time.Duration, code int, err error) {
	r.mu.Lock()
	r.s = append(r.s, sample{name, d, code, err != nil || code >= 500})
	r.mu.Unlock()
}

type client struct {
	base string
	http *http.Client
	rec  *recorder
}

func (c *client) do(name, method, path, tok string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		if name != "" {
			c.rec.add(name, time.Since(start), 0, err)
		}
		return 0, nil
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if name != "" {
		c.rec.add(name, time.Since(start), res.StatusCode, nil)
	}
	return res.StatusCode, b
}

func data(b []byte) map[string]any {
	var m struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Data
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	url, stop, err := testutil.StartPostgres()
	must(err)
	defer stop()
	must(database.MigrateUp(url, db.Migrations))
	pool, err := database.NewPostgres(ctx, url, database.Options{RLS: *rls, MaxConns: int32(*conns), SlowQuery: 20 * time.Millisecond})
	must(err)
	defer pool.Close()
	mr, err := miniredis.Run()
	must(err)
	defer mr.Close()
	flushLimits = mr.FlushAll
	cfg := config.Config{Env: "dev", JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, OTPTTL: 5 * time.Minute,
		PlatformJWTSecret: strings.Repeat("p", 40), PlatformEncKey: strings.Repeat("e", 40), PlatformAccessTTL: time.Hour, PlatformRefreshTTL: 24 * time.Hour,
		AmenityReminder: time.Hour, PaymentFake: true, MaxBodyMB: 16, SlowRequest: 10 * time.Second}
	a := app.New(app.Deps{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool,
		Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), SMS: &sms.Recorder{}})
	srv := httptest.NewServer(a.Router)
	defer srv.Close()
	tr := &http.Transport{MaxIdleConns: 1000, MaxIdleConnsPerHost: 1000, IdleConnTimeout: 90 * time.Second}
	rec := &recorder{}
	c := &client{base: srv.URL, http: &http.Client{Transport: tr, Timeout: *reqTimeout}, rec: rec}

	fmt.Printf("seeding a society with %d flats ...\n", *flats)
	t0 := time.Now()
	w := seed(ctx, c, pool, *flats, *visits, *notes)
	fmt.Printf("seeded in %s (%d residents, %d guards)\n", time.Since(t0).Round(time.Millisecond), len(w.residents), len(w.guards))

	report := &strings.Builder{}
	fmt.Fprintf(report, "# Load test\n\n- date: %s\n- hardware: %s/%s, %d CPUs, %s\n- flats: %d, residents: %d, guards: %d, historical visits: %d, notifications: %d\n- virtual users: %d for %s%s\n\n",
		time.Now().Format("2006-01-02 15:04"), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), *flats, len(w.residents), len(w.guards), *visits, *notes, *vus, *dur,
		map[bool]string{true: ", row-level security ON", false: ""}[*rls])

	if *probeFlag {
		pool1, err := database.NewPostgres(ctx, url, database.Options{RLS: *rls, MaxConns: 1})
		must(err)
		a1 := app.New(app.Deps{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool1,
			Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), SMS: &sms.Recorder{}})
		srv1 := httptest.NewServer(a1.Router)
		c1 := &client{base: srv1.URL, http: &http.Client{Transport: tr, Timeout: 3 * time.Second}, rec: &recorder{}}
		hung := probe(c1, w)
		fmt.Printf("single-connection probe: %d request kinds hang\n", len(hung))
		for _, h := range hung {
			fmt.Println("  HANGS:", h)
		}
		srv1.Close()
		pool1.Close()
		if len(hung) > 0 {
			os.Exit(3)
		}
		return
	}

	single := singleShots(ctx, c, w, srv.URL)
	fmt.Fprintf(report, "## Single-shot timings\n\n| step | time |\n|---|---|\n")
	for _, s := range single {
		fmt.Fprintf(report, "| %s | %s |\n", s.name, s.d.Round(time.Millisecond))
		fmt.Printf("%-52s %s\n", s.name, s.d.Round(time.Millisecond))
	}

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	runSteady(ctx, c, w, rec, *vus, *dur)
	runtime.GC()
	runtime.ReadMemStats(&m1)

	fmt.Fprintf(report, "\n## Steady load (%d virtual users)\n\n", *vus)
	ok := summarize(rec, report, *p95Limit)
	fmt.Fprintf(report, "\n## Statements slower than 20 ms (worst total time first)\n\n| count | total | max | statement |\n|---|---|---|---|\n")
	for i, st := range database.SlowStats() {
		if i == 12 {
			break
		}
		fmt.Fprintf(report, "| %d | %s | %s | `%s` |\n", st.Count, st.Total.Round(time.Millisecond), st.Max.Round(time.Millisecond), strings.ReplaceAll(st.SQL, "|", "\\|"))
		fmt.Printf("slow: %5dx total %8s max %6s  %.110s\n", st.Count, st.Total.Round(time.Millisecond), st.Max.Round(time.Millisecond), st.SQL)
	}
	fmt.Fprintf(report, "\nHeap after GC: %.0f MB before, %.0f MB after (goroutines %d).\n", float64(m0.HeapAlloc)/1e6, float64(m1.HeapAlloc)/1e6, runtime.NumGoroutine())
	fmt.Printf("heap before %.0f MB, after %.0f MB\n", float64(m0.HeapAlloc)/1e6, float64(m1.HeapAlloc)/1e6)
	if *out != "" {
		must(os.WriteFile(*out, []byte(report.String()), 0o644))
		fmt.Println("report written to", *out)
	}
	if !ok {
		os.Exit(2)
	}
}

// ── world ───────────────────────────────────────────────────────────────────

type user struct {
	tok  string
	unit string
}

type world struct {
	society   uuid.UUID
	admin     string
	adminUser uuid.UUID
	residents []user
	guards    []user
	units     []string
	block     string
}

// flushLimits clears the in-memory Redis so the per-IP OTP limit does not stop a bulk sign-in.
var flushLimits = func() {}

func login(c *client, phone string) string {
	flushLimits()
	c.do("", "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone})
	code, b := c.do("", "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": phone, "otp": "123456"})
	if code != 200 {
		return ""
	}
	return fmt.Sprint(data(b)["access_token"])
}

var _ = (*pgxpool.Pool)(nil)
