package app_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Nexora48/nexora-api/internal/config"
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
