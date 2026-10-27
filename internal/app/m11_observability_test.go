package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/mail"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"github.com/Nexora48/nexora-api/internal/platform/metrics"
	"github.com/Nexora48/nexora-api/internal/superadmin"
)

func scrape(t *testing.T, base, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestMetricsAreProtectedAndUseRoutePatterns(t *testing.T) {
	srv2, _ := instance(t, io.Discard, func(c *config.Config) { c.MetricsToken = "scrape-token-123"; c.RateUserPerMin = 2 })
	s := newSoc(t)

	if code, _ := scrape(t, srv2.URL, ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := scrape(t, srv2.URL, "wrong"); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	// traffic: a 200-ish call, an unauthenticated call, and a rate-limited user
	get(t, srv2.URL, "/api/v1/society/stats", s.Admin)
	get(t, srv2.URL, "/api/v1/society/stats", s.Admin)
	get(t, srv2.URL, "/api/v1/society/stats", s.Admin) // third in the minute → 429
	get(t, srv2.URL, "/api/v1/me", "")

	code, body := scrape(t, srv2.URL, "scrape-token-123")
	if code != 200 {
		t.Fatalf("scrape: %d", code)
	}
	for _, want := range []string{
		`nexora_http_requests_total{method="GET",route="/api/v1/society/stats",status="429"}`,
		`nexora_http_request_duration_seconds_bucket`,
		`nexora_rate_limited_total{limiter="user"}`,
		`nexora_db_pool_connections{state="max"}`,
		`nexora_outbox_pending`,
		`nexora_ws_connections`,
		`nexora_up 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// ids never become labels
	if strings.Contains(body, s.ID.String()) {
		t.Error("a society id leaked into a metric label")
	}
}

func TestMetricsWithoutTokenAreLoopbackOnly(t *testing.T) {
	srv2, _ := instance(t, io.Discard, nil)
	// the test server listens on loopback, so a scrape without a configured token is allowed from here
	if code, body := scrape(t, srv2.URL, ""); code != 200 || !strings.Contains(body, "nexora_up 1") {
		t.Fatalf("loopback scrape: %d", code)
	}
}

// Nothing secret may reach the logs, whatever the request carried.
func TestNoSecretsInLogs(t *testing.T) {
	var logs bytes.Buffer
	srv2, _ := instance(t, &logs, nil)
	s := newSoc(t)
	_ = srv2

	// a password login with a distinctive password, a gateway configuration with distinctive secrets,
	// a failed login and a request with a bad token
	post := func(path, tok string, body string) {
		req, _ := http.NewRequest("POST", srv2.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	post("/api/v1/auth/login", "", `{"email":"nobody@x.test","password":"Sup3r-Secret-Pass-9f2"}`)
	req, _ := http.NewRequest("PUT", srv2.URL+"/api/v1/society/payments/gateway", strings.NewReader(`{"provider":"razorpay","key_id":"rzp_test_ABC123","key_secret":"KEYSECRET-d34db33f","webhook_secret":"WHSECRET-c0ffee","active":false}`))
	req.Header.Set("Authorization", "Bearer "+s.Admin)
	req.Header.Set("Content-Type", "application/json")
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
	}
	get(t, srv2.URL, "/api/v1/me", "not-a-real-token-ZZZ")
	get(t, srv2.URL, "/api/v1/me", s.Admin)

	out := logs.String()
	if out == "" {
		t.Fatal("expected request logs")
	}
	for _, secret := range []string{"Sup3r-Secret-Pass-9f2", "KEYSECRET-d34db33f", "WHSECRET-c0ffee", "not-a-real-token-ZZZ", s.Admin, "Bearer "} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

// The check "is there room?" and the creation that follows must not interleave: 20 parallel creations at a
// limit of 5 create exactly 5.
func TestPlanLimitHoldsUnderParallelCreations(t *testing.T) {
	s := newSoc(t)
	tok := superToken(t)
	pid := newPlan(t, tok, uniq("par"), []string{"visitors"}, map[string]any{"max_guards": 5, "max_units": 7})
	assign(t, tok, s.ID, pid, false)
	bid, _ := setupBlock(t, s, "P", 1, 2) // 2 flats used, 5 left

	var wg sync.WaitGroup
	var okGuards, limitedGuards, okUnits, limitedUnits, other atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": fmt.Sprintf("Guard %d", i), "phone": nextPhone()})
			switch {
			case r.Status == 201:
				okGuards.Add(1)
			case r.errCode() == "PLAN_LIMIT":
				limitedGuards.Add(1)
			default:
				other.Add(1)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			r := call(t, "POST", "/api/v1/society/units", s.Admin, map[string]any{"block_id": bid, "floor": 2, "number": fmt.Sprintf("P2%02d", i), "unit_type": "2BHK"})
			switch {
			case r.Status == 201:
				okUnits.Add(1)
			case r.errCode() == "PLAN_LIMIT":
				limitedUnits.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if okGuards.Load() != 5 || limitedGuards.Load() != 15 {
		t.Fatalf("guards: %d created, %d refused", okGuards.Load(), limitedGuards.Load())
	}
	if okUnits.Load() != 5 || limitedUnits.Load() != 15 {
		t.Fatalf("flats: %d created, %d refused", okUnits.Load(), limitedUnits.Load())
	}
	if other.Load() != 0 {
		t.Fatalf("%d requests failed another way", other.Load())
	}
	var guards, units int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE society_id=$1 AND role_key='guard'`, s.ID).Scan(&guards)
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM units WHERE society_id=$1`, s.ID).Scan(&units)
	if guards != 5 || units != 7 {
		t.Fatalf("rows: %d guards, %d flats", guards, units)
	}
}

func TestHealthShowsBackupAgeAndCommit(t *testing.T) {
	sup := supportToken(t)
	check := func() map[string]any {
		h := sa(t, "GET", "/health", sup, nil)
		for _, c := range h.data()["checks"].([]any) {
			if m := c.(map[string]any); m["name"] == "Backups" {
				return m
			}
		}
		t.Fatal("no Backups check")
		return nil
	}
	if c := check(); c["status"] != "warn" {
		t.Fatalf("unconfigured: %v", c)
	}
	f := filepath.Join(t.TempDir(), "backup.last")
	aplat.SetBackupFile(f)
	defer aplat.SetBackupFile("")
	if c := check(); c["status"] != "fail" {
		t.Fatalf("never backed up: %v", c)
	}
	write := func(age time.Duration) {
		_ = os.WriteFile(f, []byte(strconv.FormatInt(time.Now().Add(-age).Unix(), 10)), 0o644)
	}
	write(2 * time.Hour)
	if c := check(); c["status"] != "ok" || !strings.Contains(c["detail"].(string), "restore drill never run") {
		t.Fatalf("fresh backup: %v", c)
	}
	write(30 * time.Hour)
	if c := check(); c["status"] != "warn" {
		t.Fatalf("a day-old backup: %v", c)
	}
	write(60 * time.Hour)
	if c := check(); c["status"] != "fail" {
		t.Fatalf("a stale backup: %v", c)
	}
}

// Every nexora_* metric an alert or a dashboard uses must really be exposed, or the alert can never fire.
func TestAlertsAndDashboardsOnlyUseMetricsThatExist(t *testing.T) {
	srv2, _ := instance(t, io.Discard, nil)
	get(t, srv2.URL, "/api/v1/me", "") // one request so the HTTP series exist
	hb := filepath.Join(t.TempDir(), "hb")
	_ = os.WriteFile(hb, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	metrics.BackupFile = hb
	defer func() { metrics.BackupFile = "" }()
	mustExec(t, context.Background(), `SELECT 1`)
	_, body := scrape(t, srv2.URL, "")

	exposed := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			exposed[strings.Fields(line)[2]] = true
		}
	}
	if len(exposed) < 10 {
		t.Fatalf("only %d metrics exposed", len(exposed))
	}
	// a heartbeat for the job-age series
	heartbeat.Beat("probe")
	_, body = scrape(t, srv2.URL, "")
	if !strings.Contains(body, "nexora_job_last_run_age_seconds") {
		t.Error("job age metric not exposed after a heartbeat")
	}
	exposed["nexora_job_last_run_age_seconds"] = true

	re := regexp.MustCompile(`nexora_[a-z0-9_]+`)
	files, _ := filepath.Glob(filepath.Join("..", "..", "deployments", "monitoring", "*.yml"))
	dash, _ := filepath.Glob(filepath.Join("..", "..", "deployments", "monitoring", "grafana", "dashboards", "*.json"))
	files = append(files, dash...)
	if len(files) < 4 {
		t.Fatalf("monitoring files not found: %v", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, name := range re.FindAllString(string(b), -1) {
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			if !exposed[name] && !exposed[base] && !strings.HasSuffix(name, "_total") {
				t.Errorf("%s uses %s, which the API does not expose", filepath.Base(f), name)
			}
			if strings.HasSuffix(name, "_total") && !exposed[name] && !exposed[strings.TrimSuffix(name, "_total")] {
				t.Errorf("%s uses counter %s, which the API does not expose", filepath.Base(f), name)
			}
		}
	}
}

func TestStaffInvitationIsEmailedWhenAMailerIsConfigured(t *testing.T) {
	rec := &mail.Recorder{}
	srv2, _ := instanceWith(t, io.Discard, nil, func(d *app.Deps) { d.Mailer = rec })
	_ = srv2
	boss := newStaff(t, superadmin.RoleSuper, true)
	tok := func() string {
		l := sa2(t, srv2.URL, "POST", "/auth/login", "", map[string]any{"email": boss.Email, "password": boss.Password})
		v := sa2(t, srv2.URL, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": code(t, boss.Secret, 0)})
		return v.str("access_token")
	}()
	email := fmt.Sprintf("mailed-%s@nexora.test", uuid.NewString()[:6])
	r := sa2(t, srv2.URL, "POST", "/staff", tok, map[string]any{"email": email, "full_name": "Mia Mail", "role": "platform_support"})
	if r.Status != 201 || r.data()["emailed"] != true || r.str("invite_token") == "" {
		t.Fatalf("invite: %d %v", r.Status, r.Body)
	}
	if len(rec.Sent) != 1 || rec.Sent[0].To != email || !strings.Contains(rec.Sent[0].Body, r.str("invite_token")) || !strings.Contains(rec.Sent[0].Body, "support staff") {
		t.Fatalf("mail: %+v", rec.Sent)
	}
	// without a mailer the invitation still works and says it was not emailed
	r2 := sa(t, "POST", "/staff", superToken(t), map[string]any{"email": "plain-" + email, "full_name": "Plain", "role": "platform_support"})
	if r2.Status != 201 || r2.data()["emailed"] != false {
		t.Fatalf("no mailer: %d %v", r2.Status, r2.Body)
	}
}
