package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

func TestPasswordHashVerify(t *testing.T) {
	argonMemory = 1024
	h, err := HashPassword("Admin@123")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("Admin@123", h) || VerifyPassword("wrong", h) || VerifyPassword("x", "garbage") {
		t.Fatal("verify mismatch")
	}
}

func TestPasswordPolicy(t *testing.T) {
	for p, want := range map[string]bool{"short1": false, "longpassword": false, "12345678": false, "Passw0rd1": true} {
		if ValidPasswordPolicy(p) != want {
			t.Errorf("%q want %v", p, want)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tm := NewTokenManager("0123456789abcdef0123456789abcdef", time.Minute)
	p := authctx.Principal{UserID: uuid.New(), SocietyID: uuid.New(), MembershipID: uuid.New(), Role: "guard"}
	tok, _ := tm.IssueAccess(p)
	got, err := tm.ParseAccess(tok)
	if err != nil || got != p {
		t.Fatalf("got %v err %v", got, err)
	}
	// selection token must not work as access token
	sel, _ := tm.IssueSelection(p.UserID)
	if _, err := tm.ParseAccess(sel); err == nil {
		t.Fatal("selection token accepted as access")
	}
	// wrong secret
	if _, err := NewTokenManager("ffffffffffffffffffffffffffffffff", time.Minute).ParseAccess(tok); err == nil {
		t.Fatal("accepted foreign signature")
	}
	// expired
	exp := NewTokenManager("0123456789abcdef0123456789abcdef", -time.Minute)
	old, _ := exp.IssueAccess(p)
	if _, err := tm.ParseAccess(old); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func newOTP(t *testing.T, fixed bool) (*OTPService, *sms.Recorder, *miniredis.Miniredis) {
	mr := miniredis.RunT(t)
	rec := &sms.Recorder{}
	return NewOTPService(redis.NewClient(&redis.Options{Addr: mr.Addr()}), rec, "secret", 5*time.Minute, fixed), rec, mr
}

func TestOTPFlow(t *testing.T) {
	ctx := context.Background()
	o, rec, mr := newOTP(t, true)
	if err := o.Request(ctx, "12345"); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("want invalid phone, got %v", err)
	}
	if err := o.Request(ctx, "9999999999"); err != nil || len(rec.Messages) != 1 {
		t.Fatalf("request: %v", err)
	}
	if err := o.Request(ctx, "9999999999"); !errors.Is(err, ErrOTPCooldown) {
		t.Fatalf("want cooldown, got %v", err)
	}
	if err := o.Verify(ctx, "9999999999", "000000"); !errors.Is(err, ErrOTPInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
	if err := o.Verify(ctx, "9999999999", "123456"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := o.Verify(ctx, "9999999999", "123456"); !errors.Is(err, ErrOTPExpired) {
		t.Fatalf("code must be single-use, got %v", err)
	}
	_ = mr
}

func TestOTPAttemptsBurnCode(t *testing.T) {
	ctx := context.Background()
	o, _, _ := newOTP(t, true)
	_ = o.Request(ctx, "9999999999")
	for i := 0; i < otpMaxAttempts; i++ {
		_ = o.Verify(ctx, "9999999999", "000000")
	}
	if err := o.Verify(ctx, "9999999999", "123456"); !errors.Is(err, ErrOTPExpired) {
		t.Fatalf("code should be burned after max attempts, got %v", err)
	}
}

func TestOTPHourlyLimit(t *testing.T) {
	ctx := context.Background()
	o, _, mr := newOTP(t, true)
	for i := 0; i < otpMaxPerHour; i++ {
		if err := o.Request(ctx, "9999999999"); err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		mr.Del(key("cd", "9999999999"))
	}
	if err := o.Request(ctx, "9999999999"); !errors.Is(err, ErrOTPRateLimit) {
		t.Fatalf("want rate limit, got %v", err)
	}
}

func TestRandomOTPIsSixDigits(t *testing.T) {
	o, _, _ := newOTP(t, false)
	c, _ := o.generate()
	if len(c) != 6 {
		t.Fatalf("got %q", c)
	}
}
