package app_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/superadmin"
)

// ── helpers ──────────────────────────────────────────────────────────────────

var staffSeq atomic.Int64

type staffUser struct {
	ID       uuid.UUID
	Email    string
	Password string
	Secret   string
}

const staffPass = "Staff@12345"

func newStaff(t *testing.T, role string, enrolled bool) staffUser {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs embedded Postgres)")
	}
	mr.FlushAll()
	email := fmt.Sprintf("staff%d-%s@nexora.test", staffSeq.Add(1), uuid.NewString()[:6])
	secret := ""
	if enrolled {
		var err error
		secret, _, err = superadmin.NewTOTPSecret(email)
		if err != nil {
			t.Fatal(err)
		}
	}
	id, err := superadmin.CreateUser(context.Background(), pool, saCipher, email, "Test Staff", staffPass, role, secret)
	if err != nil {
		t.Fatal(err)
	}
	return staffUser{ID: id, Email: email, Password: staffPass, Secret: secret}
}

const saBase = "/api/v1/superadmin"

func sa(t *testing.T, method, path, token string, body any) resp {
	return call(t, method, saBase+path, token, body)
}

func code(t *testing.T, secret string, stepOffset int) string {
	t.Helper()
	c, err := superadmin.CurrentTOTP(secret, time.Now(), stepOffset)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// saLogin runs both sign-in steps for an enrolled staff user.
func saLogin(t *testing.T, u staffUser, stepOffset int) resp {
	t.Helper()
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if l.Status != 200 || l.data()["mfa_required"] != true {
		t.Fatalf("login step 1: %d %v", l.Status, l.Body)
	}
	return sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": code(t, u.Secret, stepOffset)})
}

// superToken returns an access token of a fresh super_admin.
func superToken(t *testing.T) string {
	t.Helper()
	u := newStaff(t, superadmin.RoleSuper, true)
	r := saLogin(t, u, 0)
	if r.Status != 200 {
		t.Fatalf("super login: %d %v", r.Status, r.Body)
	}
	return r.str("access_token")
}

func supportToken(t *testing.T) string {
	t.Helper()
	u := newStaff(t, superadmin.RolePlatformSupport(), true)
	return saLogin(t, u, 0).str("access_token")
}

func onboardBody(name string) map[string]any {
	return map[string]any{
		"name": name, "city": "Pune", "state": "MH", "pincode": "411001",
		"admin": map[string]any{"full_name": "Meera Joshi", "email": fmt.Sprintf("meera-%s@lake.test", uuid.NewString()[:6]), "phone": nextPhone()},
	}
}

func (r resp) obj(k string) map[string]any { m, _ := r.data()[k].(map[string]any); return m }

// ── sign-in & 2FA ────────────────────────────────────────────────────────────

func TestPlatformLoginWithTOTP(t *testing.T) {
	u := newStaff(t, superadmin.RoleSuper, true)

	if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": "Wrong@12345"}); r.Status != 401 || r.errCode() != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %d %v", r.Status, r.Body)
	}
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if l.Status != 200 || l.data()["mfa_required"] != true || l.str("mfa_token") == "" || l.str("access_token") != "" {
		t.Fatalf("step 1 must not issue a session: %d %v", l.Status, l.Body)
	}
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": "000000"}); r.Status != 401 || r.errCode() != "MFA_INVALID" {
		t.Fatalf("wrong code: %d %v", r.Status, r.Body)
	}
	good := code(t, u.Secret, 0)
	v := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": good})
	if v.Status != 200 || v.str("access_token") == "" || v.str("refresh_token") == "" {
		t.Fatalf("verify: %d %v", v.Status, v.Body)
	}
	if me := sa(t, "GET", "/me", v.str("access_token"), nil); me.Status != 200 || me.str("role") != "super_admin" || me.str("email") != u.Email {
		t.Fatalf("/me: %d %v", me.Status, me.Body)
	}

	// the same code cannot be used twice
	l2 := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l2.str("mfa_token"), "code": good}); r.Status != 401 || r.errCode() != "MFA_INVALID" {
		t.Fatalf("replayed code: %d %v", r.Status, r.Body)
	}
	// the next step's code works
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l2.str("mfa_token"), "code": code(t, u.Secret, 1)}); r.Status != 200 {
		t.Fatalf("next-step code: %d %v", r.Status, r.Body)
	}

	// refresh rotation + reuse detection
	old := v.str("refresh_token")
	rf := sa(t, "POST", "/auth/refresh", "", map[string]any{"refresh_token": old})
	if rf.Status != 200 || rf.str("refresh_token") == old {
		t.Fatalf("refresh: %d %v", rf.Status, rf.Body)
	}
	if r := sa(t, "POST", "/auth/refresh", "", map[string]any{"refresh_token": old}); r.Status != 401 || r.errCode() != "TOKEN_REUSED" {
		t.Fatalf("reuse: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/auth/refresh", "", map[string]any{"refresh_token": rf.str("refresh_token")}); r.Status != 401 {
		t.Fatalf("family should be revoked: %d", r.Status)
	}
}

func TestPlatformEnrolmentAndRecoveryCodes(t *testing.T) {
	u := newStaff(t, superadmin.RoleSuper, false)
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if l.Status != 200 || l.data()["mfa_setup_required"] != true || l.str("secret") == "" || !strings.HasPrefix(l.str("otpauth_url"), "otpauth://totp/") {
		t.Fatalf("setup response: %d %v", l.Status, l.Body)
	}
	secret := l.str("secret")
	v := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": code(t, secret, 0)})
	codes, _ := v.data()["recovery_codes"].([]any)
	if v.Status != 200 || len(codes) != 8 || v.str("access_token") == "" {
		t.Fatalf("enrol: %d %v", v.Status, v.Body)
	}

	// afterwards it is a normal 2-step login, with no secret leaked again
	l2 := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if l2.data()["mfa_required"] != true || l2.str("secret") != "" {
		t.Fatalf("second login: %v", l2.Body)
	}
	rc := fmt.Sprint(codes[0])
	ok := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l2.str("mfa_token"), "recovery_code": strings.ToUpper(rc)})
	if ok.Status != 200 {
		t.Fatalf("recovery code: %d %v", ok.Status, ok.Body)
	}
	l3 := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l3.str("mfa_token"), "recovery_code": rc}); r.Status != 401 {
		t.Fatalf("recovery code reused: %d %v", r.Status, r.Body)
	}
	// the secret is stored encrypted
	var enc string
	pool.QueryRow(context.Background(), `SELECT totp_secret_enc FROM platform_users WHERE id=$1`, u.ID).Scan(&enc)
	if enc == "" || strings.Contains(enc, secret) {
		t.Fatalf("TOTP secret must be encrypted at rest: %q", enc)
	}
}

func TestPlatformLockouts(t *testing.T) {
	u := newStaff(t, superadmin.RoleSuper, true)
	for i := 0; i < 4; i++ {
		if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": "Wrong@12345"}); r.Status != 401 {
			t.Fatalf("attempt %d: %d", i, r.Status)
		}
	}
	if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": "Wrong@12345"}); r.Status != 429 || r.errCode() != "ACCOUNT_LOCKED" {
		t.Fatalf("5th: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password}); r.errCode() != "ACCOUNT_LOCKED" {
		t.Fatalf("correct password must stay locked: %v", r.Body)
	}

	u2 := newStaff(t, superadmin.RoleSuper, true)
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": u2.Email, "password": u2.Password})
	for i := 0; i < 4; i++ {
		if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": "111111"}); r.Status != 401 {
			t.Fatalf("mfa attempt %d: %d", i, r.Status)
		}
	}
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": "111111"}); r.Status != 429 {
		t.Fatalf("mfa lock: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": code(t, u2.Secret, 0)}); r.Status != 429 {
		t.Fatalf("correct code must stay locked: %d", r.Status)
	}
}

func TestDisabledStaffCannotUseSession(t *testing.T) {
	u := newStaff(t, superadmin.RoleSuper, true)
	tok := saLogin(t, u, 0).str("access_token")
	pool.Exec(context.Background(), `UPDATE platform_users SET status='disabled' WHERE id=$1`, u.ID)
	if r := sa(t, "GET", "/me", tok, nil); r.Status != 401 {
		t.Fatalf("disabled staff with a valid token: %d", r.Status)
	}
	if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password}); r.Status != 401 {
		t.Fatalf("disabled staff login: %d", r.Status)
	}
}

// ── token separation & roles ─────────────────────────────────────────────────

func TestTokenAudienceSeparation(t *testing.T) {
	tenant := newSoc(t)
	if r := sa(t, "GET", "/me", tenant.Admin, nil); r.Status != 401 {
		t.Fatalf("tenant token on /superadmin/me: %d", r.Status)
	}
	if r := sa(t, "GET", "/societies", tenant.Admin, nil); r.Status != 401 {
		t.Fatalf("tenant token on /superadmin/societies: %d", r.Status)
	}
	plat := superToken(t)
	for _, path := range []string{"/api/v1/me", "/api/v1/society/stats", "/api/v1/society/residents"} {
		if r := call(t, "GET", path, plat, nil); r.Status != 401 {
			t.Fatalf("platform token on %s: %d", path, r.Status)
		}
	}
	// an MFA token is not an access token
	u := newStaff(t, superadmin.RoleSuper, true)
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": u.Email, "password": u.Password})
	if r := sa(t, "GET", "/me", l.str("mfa_token"), nil); r.Status != 401 {
		t.Fatalf("mfa token as access token: %d", r.Status)
	}
	if r := sa(t, "GET", "/me", "", nil); r.Status != 401 {
		t.Fatalf("no token: %d", r.Status)
	}
}

func TestSupportIsReadOnly(t *testing.T) {
	sup := supportToken(t)
	sup2 := superToken(t)
	created := sa(t, "POST", "/societies", sup2, onboardBody("Readonly Test Society"))
	if created.Status != 201 {
		t.Fatalf("setup: %d %v", created.Status, created.Body)
	}
	sid := created.obj("society")["id"].(string)

	for _, path := range []string{"/societies", "/societies/" + sid, "/features", "/audit", "/stats", "/me"} {
		if r := sa(t, "GET", path, sup, nil); r.Status != 200 {
			t.Fatalf("support GET %s: %d %v", path, r.Status, r.Body)
		}
	}
	denied := []struct {
		method, path string
		body         any
	}{
		{"POST", "/societies", onboardBody("Nope")},
		{"PUT", "/societies/" + sid, map[string]any{"name": "Renamed"}},
		{"POST", "/societies/" + sid + "/suspend", map[string]any{"reason": "no"}},
		{"POST", "/societies/" + sid + "/reactivate", nil},
		{"POST", "/societies/" + sid + "/admins", map[string]any{"full_name": "X", "email": "x@x.test", "phone": nextPhone()}},
		{"PUT", "/societies/" + sid + "/features/visitors", map[string]any{"enabled": false, "reason": "no"}},
		{"POST", "/societies/" + sid + "/features/bulk", map[string]any{"features": map[string]bool{"visitors": false}, "reason": "no"}},
	}
	for _, d := range denied {
		if r := sa(t, d.method, d.path, sup, d.body); r.Status != 403 || r.errCode() != "PLATFORM_FORBIDDEN" {
			t.Fatalf("support %s %s: %d %v", d.method, d.path, r.Status, r.Body)
		}
	}
}

// ── onboarding & forced password change ──────────────────────────────────────

func TestOnboardingAndForcedPasswordChange(t *testing.T) {
	tok := superToken(t)
	body := onboardBody("Lakeview Towers")
	body["slug"] = "lakeview-" + uuid.NewString()[:6]
	r := sa(t, "POST", "/societies", tok, body)
	if r.Status != 201 {
		t.Fatalf("onboard: %d %v", r.Status, r.Body)
	}
	soc, cred := r.obj("society"), r.obj("admin_credentials")
	temp, _ := cred["temporary_password"].(string)
	if len(temp) < 8 || cred["existing_account"] != false || soc["status"] != "active" {
		t.Fatalf("credentials/society: %v", r.Body)
	}
	if feats, _ := soc["features"].([]any); len(feats) != 10 {
		t.Fatalf("all services should exist (default on): %d", len(feats))
	}
	if soc["enabled_services"] != float64(10) || soc["total_services"] != float64(10) {
		t.Fatalf("service counts: %v", soc)
	}
	// duplicate explicit slug
	dup := onboardBody("Another")
	dup["slug"] = body["slug"]
	if d := sa(t, "POST", "/societies", tok, dup); d.Status != 409 || d.errCode() != "SLUG_TAKEN" {
		t.Fatalf("dup slug: %d %v", d.Status, d.Body)
	}
	// a derived slug that collides gets a numeric suffix instead of failing
	a1 := sa(t, "POST", "/societies", tok, onboardBody("Same Name Heights"))
	a2 := sa(t, "POST", "/societies", tok, onboardBody("Same Name Heights"))
	if a1.Status != 201 || a2.Status != 201 || a1.obj("society")["slug"] == a2.obj("society")["slug"] {
		t.Fatalf("derived slugs: %d/%d %v %v", a1.Status, a2.Status, a1.obj("society")["slug"], a2.obj("society")["slug"])
	}
	// validation
	bad := onboardBody("Bad Phone Society")
	bad["admin"].(map[string]any)["phone"] = "123"
	if d := sa(t, "POST", "/societies", tok, bad); d.Status != 400 || d.errCode() != "INVALID_PHONE" {
		t.Fatalf("bad phone: %d %v", d.Status, d.Body)
	}

	// first admin signs in with the temporary password and is gated
	email := cred["email"].(string)
	login := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": temp})
	if login.Status != 200 || login.data()["must_change_password"] != true {
		t.Fatalf("admin login: %d %v", login.Status, login.Body)
	}
	at, rt := login.str("access_token"), login.str("refresh_token")
	if me := call(t, "GET", "/api/v1/me", at, nil); me.Status != 200 || me.data()["must_change_password"] != true {
		t.Fatalf("/me must stay readable: %d %v", me.Status, me.Body)
	}
	for _, path := range []string{"/api/v1/society/blocks", "/api/v1/society/stats", "/api/v1/me/unit"} {
		if g := call(t, "GET", path, at, nil); g.Status != 403 || g.errCode() != "PASSWORD_CHANGE_REQUIRED" {
			t.Fatalf("gate on %s: %d %v", path, g.Status, g.Body)
		}
	}
	// change-password validation
	if c := call(t, "POST", "/api/v1/auth/change-password", at, map[string]any{"current_password": "nope-nope1", "new_password": "Brand@New123"}); c.Status != 401 {
		t.Fatalf("wrong current: %d %v", c.Status, c.Body)
	}
	if c := call(t, "POST", "/api/v1/auth/change-password", at, map[string]any{"current_password": temp, "new_password": "onlyletters"}); c.Status != 400 || c.errCode() != "WEAK_PASSWORD" {
		t.Fatalf("weak: %d %v", c.Status, c.Body)
	}
	if c := call(t, "POST", "/api/v1/auth/change-password", at, map[string]any{"current_password": temp, "new_password": temp}); c.Status != 400 {
		t.Fatalf("same password: %d %v", c.Status, c.Body)
	}
	ch := call(t, "POST", "/api/v1/auth/change-password", at, map[string]any{"current_password": temp, "new_password": "Brand@New123"})
	if ch.Status != 200 || ch.data()["must_change_password"] != false || ch.str("access_token") == "" {
		t.Fatalf("change password: %d %v", ch.Status, ch.Body)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": rt}); rf.Status != 401 {
		t.Fatalf("old session must be revoked: %d", rf.Status)
	}
	if g := call(t, "GET", "/api/v1/society/stats", ch.str("access_token"), nil); g.Status != 200 {
		t.Fatalf("gate lifted: %d %v", g.Status, g.Body)
	}
	if old := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": temp}); old.Status != 401 {
		t.Fatalf("temp password must stop working: %d", old.Status)
	}
	if nw := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "Brand@New123"}); nw.Status != 200 || nw.data()["must_change_password"] != false {
		t.Fatalf("new password login: %d %v", nw.Status, nw.Body)
	}

	// audit trail
	sid := soc["id"].(string)
	au := sa(t, "GET", "/audit?society_id="+sid, tok, nil)
	rows, _ := au.Body["data"].([]any)
	found := false
	for _, x := range rows {
		if x.(map[string]any)["action"] == "society.create" && x.(map[string]any)["actor_type"] == "platform" {
			found = true
		}
	}
	if !found {
		t.Fatalf("society.create audit row missing: %v", au.Body)
	}
}

func TestOnboardingReusesExistingAccount(t *testing.T) {
	tok := superToken(t)
	s0 := newSoc(t)
	_, units := setupBlock(t, s0, "A", 1, 1)
	phone := nextPhone()
	addResident(t, s0, units["A101"], "owner", "Already Here", phone) // resident without a password
	body := onboardBody("Second Society")
	body["admin"].(map[string]any)["phone"] = phone
	r := sa(t, "POST", "/societies", tok, body)
	cred := r.obj("admin_credentials")
	if r.Status != 201 || cred["existing_account"] != true || cred["temporary_password"] == nil {
		t.Fatalf("existing resident without a password gets one: %d %v", r.Status, r.Body)
	}
	// a person who already has a password keeps it
	pw := cred["temporary_password"].(string)
	call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": cred["email"], "password": pw})
	body2 := onboardBody("Third Society")
	body2["admin"].(map[string]any)["phone"] = phone
	r2 := sa(t, "POST", "/societies", tok, body2)
	if r2.Status != 201 || r2.obj("admin_credentials")["temporary_password"] != nil || r2.obj("admin_credentials")["existing_account"] != true {
		t.Fatalf("existing account keeps its password: %d %v", r2.Status, r2.Body)
	}
}

// ── service switches ─────────────────────────────────────────────────────────

func TestFeatureToggleAffectsOnlyThatSociety(t *testing.T) {
	tok := superToken(t)
	a, b := newSoc(t), newSoc(t)
	if r := call(t, "GET", "/t/feature", a.Admin, nil); r.Status != 204 {
		t.Fatalf("enabled by default: %d", r.Status)
	}
	// missing reason / unknown key / unknown society
	if r := sa(t, "PUT", "/societies/"+a.ID.String()+"/features/visitors", tok, map[string]any{"enabled": false}); r.Status != 400 {
		t.Fatalf("reason required: %d", r.Status)
	}
	if r := sa(t, "PUT", "/societies/"+a.ID.String()+"/features/teleport", tok, map[string]any{"enabled": false, "reason": "test"}); r.Status != 400 || r.errCode() != "FEATURE_UNKNOWN" {
		t.Fatalf("unknown key: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "PUT", "/societies/"+uuid.NewString()+"/features/visitors", tok, map[string]any{"enabled": false, "reason": "test"}); r.Status != 404 {
		t.Fatalf("unknown society: %d", r.Status)
	}

	off := sa(t, "PUT", "/societies/"+a.ID.String()+"/features/visitors", tok, map[string]any{"enabled": false, "reason": "unpaid invoice"})
	if off.Status != 200 {
		t.Fatalf("disable: %d %v", off.Status, off.Body)
	}
	if r := call(t, "GET", "/t/feature", a.Admin, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("A must be blocked at once: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/t/feature", b.Admin, nil); r.Status != 204 {
		t.Fatalf("B must be unaffected: %d", r.Status)
	}
	if f := call(t, "GET", "/api/v1/me/features", a.Admin, nil); f.data()["visitors"] != false || f.data()["payments"] != true {
		t.Fatalf("A features: %v", f.Body)
	}
	if f := call(t, "GET", "/api/v1/me/features", b.Admin, nil); f.data()["visitors"] != true {
		t.Fatalf("B features: %v", f.Body)
	}
	det := sa(t, "GET", "/societies/"+a.ID.String(), tok, nil)
	foundOverride := false
	for _, x := range det.data()["features"].([]any) {
		f := x.(map[string]any)
		if f["key"] == "visitors" && f["override"] == false && f["enabled"] == false && f["default_enabled"] == true && f["reason"] == "unpaid invoice" {
			foundOverride = true
		}
	}
	if !foundOverride || det.data()["enabled_services"] != float64(9) {
		t.Fatalf("detail must show the override: %v", det.Body)
	}

	// audit shows who/why/before→after
	au := sa(t, "GET", "/audit?society_id="+a.ID.String()+"&action=feature.*", tok, nil)
	rows := au.Body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want exactly one feature.toggle row, got %d: %v", len(rows), au.Body)
	}
	row := rows[0].(map[string]any)
	meta := row["meta"].(map[string]any)
	ch := meta["changes"].([]any)[0].(map[string]any)
	if row["actor_name"] != "Test Staff" || meta["reason"] != "unpaid invoice" || ch["feature"] != "visitors" || ch["from"] != true || ch["to"] != false {
		t.Fatalf("audit content: %v", row)
	}

	// no-op writes no audit row; re-enable restores; bulk applies atomically
	sa(t, "PUT", "/societies/"+a.ID.String()+"/features/visitors", tok, map[string]any{"enabled": false, "reason": "again"})
	if n := len(sa(t, "GET", "/audit?society_id="+a.ID.String()+"&action=feature.*", tok, nil).Body["data"].([]any)); n != 1 {
		t.Fatalf("no-op toggle must not be audited: %d rows", n)
	}
	if r := sa(t, "PUT", "/societies/"+a.ID.String()+"/features/visitors", tok, map[string]any{"enabled": true, "reason": "paid"}); r.Status != 200 {
		t.Fatalf("enable: %d", r.Status)
	}
	if r := call(t, "GET", "/t/feature", a.Admin, nil); r.Status != 204 {
		t.Fatalf("re-enabled: %d", r.Status)
	}
	bulk := sa(t, "POST", "/societies/"+a.ID.String()+"/features/bulk", tok, map[string]any{"features": map[string]bool{"payments": false, "polls": false}, "reason": "plan downgrade"})
	if bulk.Status != 200 {
		t.Fatalf("bulk: %d %v", bulk.Status, bulk.Body)
	}
	if f := call(t, "GET", "/api/v1/me/features", a.Admin, nil); f.data()["payments"] != false || f.data()["polls"] != false || f.data()["visitors"] != true {
		t.Fatalf("bulk result: %v", f.Body)
	}
	if bad := sa(t, "POST", "/societies/"+a.ID.String()+"/features/bulk", tok, map[string]any{"features": map[string]bool{"sos": false, "nope": false}, "reason": "atomic"}); bad.Status != 400 {
		t.Fatalf("bulk with unknown key: %d", bad.Status)
	}
	if f := call(t, "GET", "/api/v1/me/features", a.Admin, nil); f.data()["sos"] != true {
		t.Fatalf("a failed bulk must change nothing: %v", f.Body)
	}
}

// ── suspend / reactivate ─────────────────────────────────────────────────────

func TestSuspendAndReactivate(t *testing.T) {
	tok := superToken(t)
	s := newSoc(t)
	// get an admin session with a refresh token
	var email string
	pool.QueryRow(context.Background(), `SELECT u.email FROM users u JOIN memberships m ON m.user_id=u.id WHERE m.society_id=$1`, s.ID).Scan(&email)
	login := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "Admin@123"})
	if login.Status != 200 {
		t.Fatalf("admin login: %d %v", login.Status, login.Body)
	}

	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/suspend", tok, map[string]any{}); r.Status != 400 {
		t.Fatalf("reason required: %d", r.Status)
	}
	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/suspend", tok, map[string]any{"reason": "contract ended"}); r.Status != 204 {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/suspend", tok, map[string]any{"reason": "again"}); r.Status != 409 || r.errCode() != "ALREADY_SUSPENDED" {
		t.Fatalf("double suspend: %d %v", r.Status, r.Body)
	}
	if m := call(t, "GET", "/api/v1/me", login.str("access_token"), nil); m.Status != 403 || m.errCode() != "SOCIETY_SUSPENDED" {
		t.Fatalf("existing token: %d %v", m.Status, m.Body)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": login.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("refresh must be dead: %d", rf.Status)
	}
	again := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "Admin@123"})
	if again.Status != 403 || again.errCode() != "SOCIETY_SUSPENDED" || !strings.Contains(strings.ToLower(fmt.Sprint(again.Body)), "paused") {
		t.Fatalf("login while suspended: %d %v", again.Status, again.Body)
	}
	det := sa(t, "GET", "/societies/"+s.ID.String(), tok, nil)
	if det.data()["status"] != "suspended" || det.data()["suspended_reason"] != "contract ended" || det.data()["suspended_at"] == nil {
		t.Fatalf("detail: %v", det.Body)
	}
	if lst := sa(t, "GET", "/societies?status=suspended&q="+s.ID.String()[:0], tok, nil); lst.Status != 200 {
		t.Fatalf("list by status: %d", lst.Status)
	}

	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/reactivate", tok, nil); r.Status != 204 {
		t.Fatalf("reactivate: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/reactivate", tok, nil); r.Status != 409 || r.errCode() != "NOT_SUSPENDED" {
		t.Fatalf("double reactivate: %d %v", r.Status, r.Body)
	}
	if ok := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "Admin@123"}); ok.Status != 200 {
		t.Fatalf("login after reactivation: %d %v", ok.Status, ok.Body)
	}
	au := sa(t, "GET", "/audit?society_id="+s.ID.String()+"&action=society.*", tok, nil)
	if n := len(au.Body["data"].([]any)); n != 2 {
		t.Fatalf("want suspend + reactivate audit rows, got %d: %v", n, au.Body)
	}
	if r := sa(t, "POST", "/societies/"+uuid.NewString()+"/suspend", tok, map[string]any{"reason": "ghost"}); r.Status != 404 {
		t.Fatalf("unknown society: %d", r.Status)
	}
}

// ── admins ───────────────────────────────────────────────────────────────────

func TestAdminManagement(t *testing.T) {
	tok := superToken(t)
	r := sa(t, "POST", "/societies", tok, onboardBody("Admin Mgmt Society"))
	sid := r.obj("society")["id"].(string)
	first := r.obj("admin_credentials")

	second := map[string]any{"full_name": "Second Admin", "email": fmt.Sprintf("second-%s@x.test", uuid.NewString()[:6]), "phone": nextPhone()}
	add := sa(t, "POST", "/societies/"+sid+"/admins", tok, second)
	if add.Status != 201 || add.str("temporary_password") == "" {
		t.Fatalf("add admin: %d %v", add.Status, add.Body)
	}
	if dup := sa(t, "POST", "/societies/"+sid+"/admins", tok, second); dup.Status != 409 || dup.errCode() != "ALREADY_ADMIN" {
		t.Fatalf("dup admin: %d %v", dup.Status, dup.Body)
	}
	if bad := sa(t, "POST", "/societies/"+sid+"/admins", tok, map[string]any{"full_name": "E", "email": first["email"], "phone": nextPhone()}); bad.Status != 409 || bad.errCode() != "EMAIL_TAKEN" {
		t.Fatalf("email taken: %d %v", bad.Status, bad.Body)
	}
	det := sa(t, "GET", "/societies/"+sid, tok, nil)
	if len(det.data()["admins"].([]any)) != 2 {
		t.Fatalf("admins list: %v", det.Body)
	}

	// reset: new temp password, old one dies, sessions die, must change again
	pw1 := first["temporary_password"].(string)
	login := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": first["email"], "password": pw1})
	rs := sa(t, "POST", "/societies/"+sid+"/admins/"+first["user_id"].(string)+"/reset-password", tok, nil)
	if rs.Status != 200 || rs.str("temporary_password") == "" || rs.str("temporary_password") == pw1 {
		t.Fatalf("reset: %d %v", rs.Status, rs.Body)
	}
	if o := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": first["email"], "password": pw1}); o.Status != 401 {
		t.Fatalf("old password must stop working: %d", o.Status)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": login.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("sessions must be revoked: %d", rf.Status)
	}
	if n := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": first["email"], "password": rs.str("temporary_password")}); n.Status != 200 || n.data()["must_change_password"] != true {
		t.Fatalf("new temp login: %d %v", n.Status, n.Body)
	}
	// a user of another society cannot be reset through this society
	other := newSoc(t)
	var otherAdmin string
	pool.QueryRow(context.Background(), `SELECT user_id FROM memberships WHERE society_id=$1`, other.ID).Scan(&otherAdmin)
	if x := sa(t, "POST", "/societies/"+sid+"/admins/"+otherAdmin+"/reset-password", tok, nil); x.Status != 404 {
		t.Fatalf("cross-society reset: %d %v", x.Status, x.Body)
	}
}

// ── list / detail / edit / stats ─────────────────────────────────────────────

func TestSocietyListDetailEditStats(t *testing.T) {
	tok := superToken(t)
	tag := uuid.NewString()[:6]
	var ids []string
	for i := 0; i < 3; i++ {
		b := onboardBody(fmt.Sprintf("Listing %s %d", tag, i))
		if i == 2 {
			b["status"] = "trial"
		}
		r := sa(t, "POST", "/societies", tok, b)
		if r.Status != 201 {
			t.Fatalf("create %d: %d %v", i, r.Status, r.Body)
		}
		ids = append(ids, r.obj("society")["id"].(string))
	}
	rows, r := callList(t, saBase+"/societies?q=Listing+"+tag+"&page_size=2", tok)
	if r.Status != 200 || len(rows) != 2 || r.total() != 3 {
		t.Fatalf("paged search: %d rows, total %d (%v)", len(rows), r.total(), r.Body)
	}
	if rows, r := callList(t, saBase+"/societies?q=Listing+"+tag+"&status=trial", tok); len(rows) != 1 || r.total() != 1 || rows[0]["status"] != "trial" {
		t.Fatalf("status filter: %v", rows)
	}
	if _, r := callList(t, saBase+"/societies?status=bogus", tok); r.Status != 400 {
		t.Fatalf("bad status: %d", r.Status)
	}

	// detail with counts from M2 data
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 2, 3)
	addResident(t, s, units["A101"], "owner", "Counted One", nextPhone())
	addResident(t, s, units["A102"], "owner", "Counted Two", nextPhone())
	det := sa(t, "GET", "/societies/"+s.ID.String(), tok, nil)
	if det.data()["blocks"] != float64(1) || det.data()["flats"] != float64(6) || det.data()["residents"] != float64(2) {
		t.Fatalf("counts: %v", det.Body)
	}

	// edit
	up := sa(t, "PUT", "/societies/"+ids[0], tok, map[string]any{"name": "Renamed " + tag, "address": "12 Lake Road", "city": "Mumbai", "state": "MH", "pincode": "400001"})
	if up.Status != 200 || up.str("name") != "Renamed "+tag || up.str("city") != "Mumbai" || up.str("address") != "12 Lake Road" {
		t.Fatalf("edit: %d %v", up.Status, up.Body)
	}
	if r := sa(t, "PUT", "/societies/"+uuid.NewString(), tok, map[string]any{"name": "Ghost"}); r.Status != 404 {
		t.Fatalf("edit unknown: %d", r.Status)
	}

	st := sa(t, "GET", "/stats", tok, nil)
	if st.Status != 200 || st.data()["societies"].(float64) < 4 || st.data()["trial"].(float64) < 1 {
		t.Fatalf("stats: %v", st.Body)
	}
	cat := sa(t, "GET", "/features", tok, nil)
	if got, _ := cat.Body["data"].([]any); len(got) != 10 {
		t.Fatalf("catalogue: %v", cat.Body)
	}
	// audit filters
	if _, r := callList(t, saBase+"/audit?actor=not-a-uuid", tok); r.Status != 400 {
		t.Fatalf("bad actor filter: %d", r.Status)
	}
	if rows, r := callList(t, saBase+"/audit?action=society.create&page_size=1", tok); r.Status != 200 || len(rows) != 1 || r.total() < 3 {
		t.Fatalf("audit list: %v", r.Body)
	}
	future := time.Now().Add(48 * time.Hour).Format("2006-01-02")
	if rows, _ := callList(t, saBase+"/audit?from="+future, tok); len(rows) != 0 {
		t.Fatalf("from filter: %d rows", len(rows))
	}
	_ = http.StatusOK
}
