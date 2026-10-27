package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"github.com/Nexora48/nexora-api/internal/usage"
	"github.com/go-chi/chi/v5"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/db"
	"github.com/Nexora48/nexora-api/internal/amenity"
	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/attendance"
	"github.com/Nexora48/nexora-api/internal/billing"
	"github.com/Nexora48/nexora-api/internal/complaint"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/notice"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/payment"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/poll"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/seed"
	"github.com/Nexora48/nexora-api/internal/sos"
	"github.com/Nexora48/nexora-api/internal/superadmin"
	"github.com/Nexora48/nexora-api/internal/testutil"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

var (
	pool *pgxpool.Pool
	mr   *miniredis.Miniredis
	srv  *httptest.Server

	saCipher    *superadmin.Cipher
	aplat       *superadmin.Service
	arouter     *chi.Mux
	testDBURL   string
	ausage      *usage.Service
	vsvc        *visitor.Service
	ahub        *realtime.Hub
	asos        *sos.Service
	anotices    *notice.Service
	acomplaints *complaint.Service
	apolls      *poll.Service
	anote       *notify.Service
	aamenity    *amenity.Service
	aattend     *attendance.Service
	abill       *billing.Service
	apay        *payment.Service
	pushRec     *pushRecorder
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	url, stop, err := testutil.StartPostgres()
	if err != nil {
		panic(err)
	}
	testDBURL = url
	ctx := context.Background()
	if err := database.MigrateUp(url, db.Migrations); err != nil {
		panic(err)
	}
	// TEST_RLS=1 runs the whole suite through the row-level-security role (see `make test-rls`)
	pool, err = database.NewPostgres(ctx, url, database.Options{RLS: os.Getenv("TEST_RLS") == "1", MaxConns: testMaxConns()})
	if err != nil {
		panic(err)
	}
	if err := seed.Dev(ctx, pool); err != nil {
		panic(err)
	}
	mr, _ = miniredis.Run()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	cfg := config.Config{Env: "test", JWTSecret: strings.Repeat("k", 40),
		AccessTTL: 15 * 60 * 1e9, RefreshTTL: 30 * 24 * 3600 * 1e9, OTPTTL: 5 * 60 * 1e9,
		PlatformJWTSecret: strings.Repeat("p", 40), PlatformEncKey: strings.Repeat("e", 40),
		PlatformAccessTTL: 15 * 60 * 1e9, PlatformRefreshTTL: 7 * 24 * 3600 * 1e9, AmenityReminder: time.Hour, PaymentFake: true}
	var cerr error
	saCipher, cerr = superadmin.NewCipher(cfg.PlatformEncKey)
	if cerr != nil {
		panic(cerr)
	}
	blobDir, _ := os.MkdirTemp("", "nexora-blobs")
	cfg.BlobPath = blobDir
	pushRec = &pushRecorder{}
	a := app.New(app.Deps{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Redis: rdb, SMS: &sms.Recorder{}, Push: pushRec})
	vsvc = a.Visitors
	ahub = a.Hub
	asos = a.Sos
	anotices = a.Notices
	acomplaints = a.Complaints
	apolls = a.Polls
	anote = a.Notify
	aamenity = a.Amenities
	aattend = a.Attendance
	abill = a.Billing
	apay = a.Payments
	aplat = a.Platform
	arouter = a.Router
	ausage = a.Usage
	// Test-only guarded routes exercising the middleware chain.
	a.Router.With(a.Protect("visitor.approve", "")...).Get("/t/perm", ok)
	a.Router.With(a.Protect("", "visitors")...).Get("/t/feature", ok)
	srv = httptest.NewServer(a.Router)

	code := m.Run()
	srv.Close()
	pool.Close()
	stop()
	_ = os.RemoveAll(blobDir)
	os.Exit(code)
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// ── helpers ──────────────────────────────────────────────────────────────────

type resp struct {
	Status int
	Body   map[string]any
}

func (r resp) data() map[string]any { d, _ := r.Body["data"].(map[string]any); return d }
func (r resp) errCode() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}
func (r resp) str(k string) string { s, _ := r.data()[k].(string); return s }

func call(t *testing.T, method, path, token string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return resp{res.StatusCode, m}
}

func reset(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs embedded Postgres)")
	}
	mr.FlushAll()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE societies SET status='active'`,
		`DELETE FROM society_features`,
		`DELETE FROM refresh_tokens`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func residentLogin(t *testing.T) resp {
	t.Helper()
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": seed.ResidentPhone}); r.Status != 204 {
		t.Fatalf("otp request: %d %v", r.Status, r.Body)
	}
	return call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{
		"phone": seed.ResidentPhone, "otp": "123456",
		"device": map[string]any{"device_id": "dev-1", "platform": "ios", "app_version": "1.0.0"}})
}

func adminLogin(t *testing.T, pass string) resp {
	t.Helper()
	return call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": seed.AdminEmail, "password": pass})
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestResidentOTPLoginAndMe(t *testing.T) {
	reset(t)
	r := residentLogin(t)
	if r.Status != 200 || r.str("access_token") == "" || r.str("refresh_token") == "" {
		t.Fatalf("login: %d %v", r.Status, r.Body)
	}
	me := call(t, "GET", "/api/v1/me", r.str("access_token"), nil)
	if me.Status != 200 {
		t.Fatalf("me: %d %v", me.Status, me.Body)
	}
	d := me.data()
	if d["society"].(map[string]any)["name"] != "Sunrise Heights" ||
		d["membership"].(map[string]any)["role"] != "resident_owner" ||
		d["user"].(map[string]any)["full_name"] != "Rajesh Kumar" {
		t.Fatalf("unexpected me: %v", d)
	}
	perms, _ := json.Marshal(d["permissions"])
	if !strings.Contains(string(perms), "visitor.create") || strings.Contains(string(perms), "society.manage") {
		t.Fatalf("permissions wrong: %s", perms)
	}
	if d["features"].(map[string]any)["visitors"] != true {
		t.Fatal("visitors feature should default on")
	}
}

func TestOTPErrors(t *testing.T) {
	reset(t)
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": "8888888888"}); r.Status != 404 || r.errCode() != "PHONE_NOT_REGISTERED" {
		t.Fatalf("unregistered: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": "123"}); r.Status != 400 || r.errCode() != "INVALID_PHONE" {
		t.Fatalf("invalid: %d %v", r.Status, r.Body)
	}
	call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": seed.ResidentPhone})
	r := call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": seed.ResidentPhone, "otp": "000000"})
	if r.Status != 401 || r.errCode() != "OTP_INVALID" {
		t.Fatalf("wrong otp: %d %v", r.Status, r.Body)
	}
}

func TestAdminPasswordLoginAndLockout(t *testing.T) {
	reset(t)
	r := adminLogin(t, seed.AdminPass)
	if r.Status != 200 || r.data()["membership"].(map[string]any)["role"] != "society_admin" {
		t.Fatalf("admin login: %d %v", r.Status, r.Body)
	}
	for i := 0; i < 4; i++ {
		if r := adminLogin(t, "Wrong@123"); r.Status != 401 || r.errCode() != "INVALID_CREDENTIALS" {
			t.Fatalf("attempt %d: %d %v", i, r.Status, r.Body)
		}
	}
	if r := adminLogin(t, "Wrong@123"); r.Status != 429 || r.errCode() != "ACCOUNT_LOCKED" {
		t.Fatalf("5th: %d %v", r.Status, r.Body)
	}
	if r := adminLogin(t, seed.AdminPass); r.errCode() != "ACCOUNT_LOCKED" {
		t.Fatalf("correct password must stay locked: %v", r.Body)
	}
}

func TestResidentCannotUseAdminLogin(t *testing.T) {
	reset(t)
	ctx := context.Background()
	hash, _ := hashFor("Resident@123")
	uid := uuid.New()
	pool.Exec(ctx, `INSERT INTO users (id, full_name, phone, email, password_hash) VALUES ($1,'Pw Resident','7777777771','pwres@sunrise.test',$2)`, uid, hash)
	pool.Exec(ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'resident_owner')`, uid, seed.SocietyID)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid) })

	r := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": "pwres@sunrise.test", "password": "Resident@123"})
	if r.Status != 403 || r.errCode() != "NOT_AUTHORISED" {
		t.Fatalf("resident on admin app: %d %v", r.Status, r.Body)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	reset(t)
	r := residentLogin(t)
	old := r.str("refresh_token")

	r2 := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": old})
	if r2.Status != 200 || r2.str("refresh_token") == old || r2.str("access_token") == "" {
		t.Fatalf("refresh: %d %v", r2.Status, r2.Body)
	}
	newTok := r2.str("refresh_token")

	// replay of the rotated token = theft signal
	if r3 := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": old}); r3.Status != 401 || r3.errCode() != "TOKEN_REUSED" {
		t.Fatalf("reuse: %d %v", r3.Status, r3.Body)
	}
	// ...which also kills the legitimate descendant
	if r4 := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": newTok}); r4.Status != 401 {
		t.Fatalf("family should be revoked: %d %v", r4.Status, r4.Body)
	}
	if r5 := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": "nonsense"}); r5.Status != 401 || r5.errCode() != "TOKEN_INVALID" {
		t.Fatalf("garbage: %d %v", r5.Status, r5.Body)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	reset(t)
	r := residentLogin(t)
	if out := call(t, "POST", "/api/v1/auth/logout", r.str("access_token"), map[string]any{"refresh_token": r.str("refresh_token")}); out.Status != 204 {
		t.Fatalf("logout: %d %v", out.Status, out.Body)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": r.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("refresh after logout: %d", rf.Status)
	}
}

func TestMeRequiresValidToken(t *testing.T) {
	reset(t)
	if r := call(t, "GET", "/api/v1/me", "", nil); r.Status != 401 || r.errCode() != "UNAUTHORIZED" {
		t.Fatalf("no token: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/api/v1/me", "garbage", nil); r.Status != 401 {
		t.Fatalf("bad token: %d", r.Status)
	}
}

func TestRBAC(t *testing.T) {
	reset(t)
	res := residentLogin(t)
	adm := adminLogin(t, seed.AdminPass)
	if r := call(t, "GET", "/t/perm", res.str("access_token"), nil); r.Status != 204 {
		t.Fatalf("resident has visitor.approve: %d", r.Status)
	}
	// guard lacks visitor.approve
	ctx := context.Background()
	gid, mid := uuid.New(), uuid.New()
	pool.Exec(ctx, `INSERT INTO users (id, full_name, phone) VALUES ($1,'Guard','7777777772')`, gid)
	pool.Exec(ctx, `INSERT INTO memberships (id, user_id, society_id, role_key) VALUES ($1,$2,$3,'guard')`, mid, gid, seed.SocietyID)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, gid) })
	call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": "7777777772"})
	g := call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": "7777777772", "otp": "123456"})
	if r := call(t, "GET", "/t/perm", g.str("access_token"), nil); r.Status != 403 || r.errCode() != "FORBIDDEN" {
		t.Fatalf("guard should be forbidden: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/t/perm", adm.str("access_token"), nil); r.Status != 204 {
		t.Fatalf("admin allowed: %d", r.Status)
	}
}

func TestFeatureFlag(t *testing.T) {
	reset(t)
	tok := residentLogin(t).str("access_token")
	if r := call(t, "GET", "/t/feature", tok, nil); r.Status != 204 {
		t.Fatalf("enabled: %d", r.Status)
	}
	pool.Exec(context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'visitors',false)`, seed.SocietyID)
	if r := call(t, "GET", "/t/feature", tok, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("disabled: %d %v", r.Status, r.Body)
	}
	f := call(t, "GET", "/api/v1/me/features", tok, nil)
	if f.data()["visitors"] != false || f.data()["payments"] != true {
		t.Fatalf("features map: %v", f.Body)
	}
}

func TestSuspendedSociety(t *testing.T) {
	reset(t)
	tok := residentLogin(t).str("access_token")
	pool.Exec(context.Background(), `UPDATE societies SET status='suspended'`)
	if r := call(t, "GET", "/api/v1/me", tok, nil); r.Status != 403 || r.errCode() != "SOCIETY_SUSPENDED" {
		t.Fatalf("existing token: %d %v", r.Status, r.Body)
	}
	mr.FlushAll()
	call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": seed.ResidentPhone})
	r := call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": seed.ResidentPhone, "otp": "123456"})
	if r.Status != 403 || r.errCode() != "SOCIETY_SUSPENDED" {
		t.Fatalf("login: %d %v", r.Status, r.Body)
	}
}

func TestMultiSocietySelectionAndTenantIsolation(t *testing.T) {
	reset(t)
	ctx := context.Background()
	soc2, mem2 := uuid.New(), uuid.New()
	pool.Exec(ctx, `INSERT INTO societies (id, name, slug) VALUES ($1,'Lakeview Towers','lakeview-towers')`, soc2)
	pool.Exec(ctx, `INSERT INTO memberships (id, user_id, society_id, role_key) VALUES ($1,$2,$3,'resident_tenant')`, mem2, seed.ResidentID, soc2)
	pool.Exec(ctx, `UPDATE society_features SET enabled=false WHERE false`)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM societies WHERE id=$1`, soc2)
	})

	r := residentLogin(t)
	if r.data()["selection_required"] != true || len(r.data()["memberships"].([]any)) != 2 || r.str("access_token") != "" {
		t.Fatalf("expected selection: %v", r.Body)
	}
	sel := r.str("selection_token")

	// another user's membership can never be selected
	if x := call(t, "POST", "/api/v1/auth/select-society", "", map[string]any{
		"selection_token": sel, "membership_id": seed.AdminMemID}); x.Status != 403 {
		t.Fatalf("foreign membership: %d %v", x.Status, x.Body)
	}
	// a selection token is not an access token
	if x := call(t, "GET", "/api/v1/me", sel, nil); x.Status != 401 {
		t.Fatalf("selection token used as access: %d", x.Status)
	}

	s := call(t, "POST", "/api/v1/auth/select-society", "", map[string]any{"selection_token": sel, "membership_id": mem2.String()})
	if s.Status != 200 || s.str("access_token") == "" {
		t.Fatalf("select: %d %v", s.Status, s.Body)
	}
	me := call(t, "GET", "/api/v1/me", s.str("access_token"), nil).data()
	if me["society"].(map[string]any)["name"] != "Lakeview Towers" || me["membership"].(map[string]any)["role"] != "resident_tenant" {
		t.Fatalf("tenant context wrong: %v", me)
	}
	// Disabling a feature in society 1 must not leak into society 2.
	pool.Exec(ctx, `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'visitors',false)`, seed.SocietyID)
	if r := call(t, "GET", "/t/feature", s.str("access_token"), nil); r.Status != 204 {
		t.Fatalf("society 2 must be unaffected: %d", r.Status)
	}
}

func TestRegisterDevice(t *testing.T) {
	reset(t)
	tok := residentLogin(t).str("access_token")
	r := call(t, "PUT", "/api/v1/me/devices", tok, map[string]any{"device_id": "dev-1", "platform": "ios", "fcm_token": "tok123", "app_version": "1.0.1"})
	if r.Status != 204 {
		t.Fatalf("device: %d %v", r.Status, r.Body)
	}
	var n int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE device_id='dev-1' AND fcm_token='tok123'`).Scan(&n)
	if n != 1 {
		t.Fatalf("device row missing: %d", n)
	}
}

// TEST_MAXCONNS=1 runs the suite through a one-connection pool: any code that holds a connection while asking
// for a second one then hangs, which is how the pool-deadlock probe in `make test-pool1` finds it.
func testMaxConns() int32 {
	n, _ := strconv.Atoi(os.Getenv("TEST_MAXCONNS"))
	return int32(n)
}
