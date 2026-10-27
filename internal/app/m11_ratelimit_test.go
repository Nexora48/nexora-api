package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
)

// limited builds a second API instance with tiny limits and its own Redis, on the shared test database.
func limited(t *testing.T, ipPerMin, userPerMin int) (*httptest.Server, *miniredis.Miniredis) {
	t.Helper()
	return instance(t, io.Discard, func(c *config.Config) { c.RateIPPerMin, c.RateUserPerMin = ipPerMin, userPerMin })
}

// instance builds another API on the shared database with its own Redis and a log writer.
func instance(t *testing.T, logw io.Writer, tweak func(*config.Config)) (*httptest.Server, *miniredis.Miniredis) {
	t.Helper()
	return instanceWith(t, logw, tweak, nil)
}

// instanceWith also lets a test adjust the dependencies (for example the mailer).
func instanceWith(t *testing.T, logw io.Writer, tweak func(*config.Config), deps func(*app.Deps)) (*httptest.Server, *miniredis.Miniredis) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs embedded Postgres)")
	}
	mr2 := miniredis.RunT(t)
	cfg := config.Config{Env: "test", JWTSecret: strings.Repeat("k", 40),
		AccessTTL: 15 * 60 * 1e9, RefreshTTL: 30 * 24 * 3600 * 1e9, OTPTTL: 5 * 60 * 1e9,
		PlatformJWTSecret: strings.Repeat("p", 40), PlatformEncKey: strings.Repeat("e", 40),
		PlatformAccessTTL: 15 * 60 * 1e9, PlatformRefreshTTL: 7 * 24 * 3600 * 1e9, PaymentFake: true,
		MaxBodyMB: 1}
	if tweak != nil {
		tweak(&cfg)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	d := app.Deps{Cfg: cfg, Log: slog.New(slog.NewTextHandler(logw, nil)), Pool: pool, Redis: rdb, SMS: &sms.Recorder{}}
	if deps != nil {
		deps(&d)
	}
	a := app.New(d)
	srv2 := httptest.NewServer(a.Router)
	t.Cleanup(srv2.Close)
	return srv2, mr2
}

func get(t *testing.T, base, path, tok string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestGlobalIPLimitAnswersWithRetryAfter(t *testing.T) {
	srv2, _ := limited(t, 5, 0)
	for i := 1; i <= 5; i++ {
		res := get(t, srv2.URL, "/api/v1/me", "")
		if res.StatusCode != 401 { // reaches the handler (no token), so it was not limited
			t.Fatalf("request %d: %d", i, res.StatusCode)
		}
		if res.Header.Get("X-RateLimit-Limit") != "5" || res.Header.Get("X-RateLimit-Remaining") != strconv.Itoa(5-i) {
			t.Fatalf("headers %d: %v", i, res.Header)
		}
	}
	res := get(t, srv2.URL, "/api/v1/me", "")
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("6th request: %d %v", res.StatusCode, res.Header)
	}
	// probes are never limited
	for i := 0; i < 20; i++ {
		if r := get(t, srv2.URL, "/health", ""); r.StatusCode != 200 {
			t.Fatalf("health limited: %d", r.StatusCode)
		}
	}
}

func TestUserLimitIsPerUserNotPerIP(t *testing.T) {
	srv2, _ := limited(t, 0, 3)
	s := newSoc(t)
	other := newSoc(t)
	for i := 0; i < 3; i++ {
		if r := get(t, srv2.URL, "/api/v1/society/stats", s.Admin); r.StatusCode == 429 {
			t.Fatalf("limited too early at %d", i)
		}
	}
	if r := get(t, srv2.URL, "/api/v1/society/stats", s.Admin); r.StatusCode != 429 {
		t.Fatalf("4th request as the same user: %d", r.StatusCode)
	}
	if r := get(t, srv2.URL, "/api/v1/society/stats", other.Admin); r.StatusCode == 429 {
		t.Fatal("another user from the same IP was limited")
	}
}

func TestRedisOutageFailsOpenForGeneralLimitsAndClosedForSignIn(t *testing.T) {
	srv2, mr2 := limited(t, 100, 100)
	s := newSoc(t)
	mr2.Close() // Redis is gone
	if r := get(t, srv2.URL, "/api/v1/society/stats", s.Admin); r.StatusCode == 429 || r.StatusCode >= 500 {
		t.Fatalf("general routes must keep working without the limiter: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv2.URL+"/api/v1/auth/otp/request", stringsReader(`{"phone":"9999999999"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("sign-in must refuse when its limiter is down: %d", res.StatusCode)
	}
	_ = context.Background()
}

func TestBodyCeilingAppliesToTheWholeAPI(t *testing.T) {
	srv2, _ := limited(t, 0, 0)
	big := make([]byte, 2<<20)
	req, _ := http.NewRequest("POST", srv2.URL+"/api/v1/auth/login", bytesReader(big))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 && res.StatusCode != 413 {
		t.Fatalf("2 MB body: %d", res.StatusCode)
	}
}

func stringsReader(s string) io.Reader { return bytes.NewReader([]byte(s)) }
func bytesReader(b []byte) io.Reader   { return bytes.NewReader(b) }

// sa2 calls the SuperAdmin API of another instance.
func sa2(t *testing.T, base, method, path, tok string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+saBase+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return resp{res.StatusCode, m}
}
