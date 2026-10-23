package superadmin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher("test-key")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := c.Encrypt("JBSWY3DPEHPK3PXP")
	if enc == "JBSWY3DPEHPK3PXP" {
		t.Fatal("not encrypted")
	}
	if got, err := c.Decrypt(enc); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("got %q %v", got, err)
	}
	other, _ := NewCipher("another-key")
	if _, err := other.Decrypt(enc); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	enc2, _ := c.Encrypt("JBSWY3DPEHPK3PXP")
	if enc == enc2 {
		t.Fatal("nonce must differ per encryption")
	}
}

func TestTOTPMatchAndSkew(t *testing.T) {
	secret, url, err := NewTOTPSecret("a@b.c")
	if err != nil || secret == "" || url == "" {
		t.Fatalf("secret: %v", err)
	}
	now := time.Unix(1_800_000_000, 0)
	code, _ := CurrentTOTP(secret, now, 0)
	step, ok := MatchTOTP(secret, code, now)
	if !ok || step != now.Unix()/30 {
		t.Fatalf("current step: %v %v", step, ok)
	}
	prev, _ := CurrentTOTP(secret, now, -1)
	next, _ := CurrentTOTP(secret, now, 1)
	if _, ok := MatchTOTP(secret, prev, now); !ok {
		t.Fatal("previous step should be accepted (clock skew)")
	}
	if _, ok := MatchTOTP(secret, next, now); !ok {
		t.Fatal("next step should be accepted (clock skew)")
	}
	old, _ := CurrentTOTP(secret, now, -3)
	if _, ok := MatchTOTP(secret, old, now); ok {
		t.Fatal("a code from 3 steps ago must be rejected")
	}
	if _, ok := MatchTOTP(secret, "000000", now); ok && code != "000000" {
		t.Fatal("wrong code accepted")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes(8)
	re := regexp.MustCompile(`^[a-z0-9]{5}-[a-z0-9]{5}$`)
	seen := map[string]bool{}
	for _, c := range codes {
		if !re.MatchString(c) || seen[c] {
			t.Fatalf("bad or duplicate code %q", c)
		}
		seen[c] = true
	}
	if hashCode(" "+codes[0]+" ") != hashCode(codes[0]) {
		t.Fatal("hash must ignore whitespace/case")
	}
}

func TestTokenAudiences(t *testing.T) {
	tk := NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	id := uuid.New()
	access, _ := tk.IssueAccess(id, RoleSuper)
	mfa, _ := tk.IssueMFA(id)
	if p, err := tk.ParseAccess(access); err != nil || p.UserID != id || p.Role != RoleSuper {
		t.Fatalf("access: %v %v", p, err)
	}
	if _, err := tk.ParseAccess(mfa); err == nil {
		t.Fatal("MFA token accepted as access token")
	}
	if _, err := tk.ParseMFA(access); err == nil {
		t.Fatal("access token accepted as MFA token")
	}
	if _, err := NewTokens("ffffffffffffffffffffffffffffffff", time.Minute).ParseAccess(access); err == nil {
		t.Fatal("foreign signature accepted")
	}
	expired := NewTokens("0123456789abcdef0123456789abcdef", -time.Minute)
	old, _ := expired.IssueAccess(id, RoleSuper)
	if _, err := tk.ParseAccess(old); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestIPAllowlist(t *testing.T) {
	h := IPAllowlist([]string{"10.0.0.0/8", " 192.168.5.0/24 "})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for addr, want := range map[string]int{"10.1.2.3:5555": 204, "192.168.5.9:1": 204, "192.168.6.9:1": 403, "8.8.8.8:80": 403} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: want %d got %d", addr, want, rec.Code)
		}
	}
	open := IPAllowlist(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 204 {
		t.Fatal("empty allowlist must allow all")
	}
}

func TestSlugAndPassword(t *testing.T) {
	if got := slugify("  Lakeview Towers & Co. "); got != "lakeview-towers-co" {
		t.Fatalf("slug %q", got)
	}
	if got := slugify("!!!"); got != "society" {
		t.Fatalf("empty slug %q", got)
	}
	for i := 0; i < 50; i++ {
		pw, err := tempPassword()
		if err != nil || len(pw) != 12 {
			t.Fatalf("temp password %q %v", pw, err)
		}
	}
}
