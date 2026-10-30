package config

import (
	"strings"
	"testing"
)

func safeProd() Config {
	return Config{
		Env: "prod", DatabaseURL: "postgres://nx:S3cretPw@db.internal:5432/nexora?sslmode=require",
		JWTSecret: strings.Repeat("a", 40), PlatformJWTSecret: strings.Repeat("b", 40), PlatformEncKey: strings.Repeat("c", 40),
		CORSOrigins: []string{"https://admin.example.com"}, PublicBaseURL: "https://api.example.com", RateIPPerMin: 600,
	}
}

func TestValidateAcceptsSafeProduction(t *testing.T) {
	if err := safeProd().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSkipsDev(t *testing.T) {
	c := Config{Env: "dev", PaymentFake: true, CORSOrigins: []string{"*"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("dev must pass: %v", err)
	}
}

func TestValidateRefusesUnsafeProduction(t *testing.T) {
	cases := map[string]func(*Config){
		"development value":     func(c *Config) { c.JWTSecret = "dev-secret-change-me-dev-secret-change-me" },
		"must differ":           func(c *Config) { c.PlatformJWTSecret = c.JWTSecret },
		"fake payment":          func(c *Config) { c.PaymentFake = true },
		"CORS_ORIGINS is empty": func(c *Config) { c.CORSOrigins = nil },
		"must not contain *":    func(c *Config) { c.CORSOrigins = []string{"*"} },
		"default password":      func(c *Config) { c.DatabaseURL = "postgres://nexora:nexora@db:5432/x?sslmode=require" },
		"sslmode=disable":       func(c *Config) { c.DatabaseURL = "postgres://a:b@db.internal/x?sslmode=disable" },
		"https://":              func(c *Config) { c.PublicBaseURL = "http://api.example.com" },
		"switches the per-IP":   func(c *Config) { c.RateIPPerMin = 0 },
		"PLATFORM_ENC_KEY look": func(c *Config) { c.PlatformEncKey = "dev-platform-enc-key-not-for-production" },
	}
	for want, mutate := range cases {
		c := safeProd()
		mutate(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v", want, err)
		}
	}
	// several problems are reported together
	c := safeProd()
	c.PaymentFake, c.CORSOrigins = true, nil
	if err := c.Validate(); err == nil || strings.Count(err.Error(), "\n - ") < 2 {
		t.Errorf("expected a list: %v", err)
	}
}

func TestFixedMFACodeIsRefusedOutsideDev(t *testing.T) {
	for _, env := range []string{"prod", "staging", "test"} {
		t.Setenv("APP_ENV", env)
		t.Setenv("DEV_FIXED_MFA", "123456")
		t.Setenv("JWT_SECRET", strings.Repeat("a", 40))
		t.Setenv("PLATFORM_JWT_SECRET", strings.Repeat("b", 40))
		t.Setenv("PLATFORM_ENC_KEY", strings.Repeat("c", 40))
		if _, err := Load(); err == nil {
			t.Fatalf("DEV_FIXED_MFA must be refused when APP_ENV=%s", env)
		}
	}
	t.Setenv("APP_ENV", "dev")
	if c, err := Load(); err != nil || c.DevFixedMFA != "123456" {
		t.Fatalf("dev: %v %q", err, c.DevFixedMFA)
	}
	t.Setenv("DEV_FIXED_MFA", "12ab56")
	if _, err := Load(); err == nil {
		t.Fatal("a non-numeric code must be refused")
	}
}
