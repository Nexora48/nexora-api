package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string
	HTTPAddr    string
	DatabaseURL string
	RedisAddr   string
	JWTSecret   string
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	OTPTTL      time.Duration
	AutoMigrate bool
	CORSOrigins []string

	// SuperAdmin (platform) console
	PlatformJWTSecret    string
	PlatformEncKey       string
	DevFixedMFA          string // APP_ENV=dev only: this 6-digit code is accepted for every staff 2FA prompt
	PlatformAllowedCIDRs []string
	PlatformAccessTTL    time.Duration
	PlatformRefreshTTL   time.Duration

	// Visitors
	VisitorMaxWindow  time.Duration
	VisitorWalkinWait time.Duration
	ShutdownTimeout   time.Duration

	// Push + media (M5). Empty values select the dev implementations.
	FCMProjectID       string
	FCMCredentialsFile string
	BlobPath           string // local-disk photo store; empty disables walk-in photos

	// SOS and notifications (M6)
	SOSEscalateAfter time.Duration
	SOSCancelWindow  time.Duration
	NotifyRetention  time.Duration

	// Community (M7)
	ComplaintAutoclose time.Duration
	AmenityReminder    time.Duration
	PublicBaseURL      string // where phones reach the API (hosted checkout links)
	PaymentFake        bool   // use the test gateway when a society has none configured
	ComplaintReopen    time.Duration
	AttachmentMaxMB    int

	// Hardening (M11)
	TrustProxy          bool          // honour X-Forwarded-For / X-Real-IP (only behind a proxy you control)
	MaxBodyMB           int           // hard ceiling for any request body
	SlowRequest         time.Duration // requests slower than this are logged as warnings
	RateIPPerMin        int           // per-IP requests per minute (0 = off)
	RateUserPerMin      int           // per-user requests per minute (0 = off)
	MetricsToken        string        // bearer token for /metrics (empty = only loopback may scrape)
	SentryDSN           string
	SMTPHost            string
	SMTPPort            int
	SMTPUser            string
	SMTPPass            string
	MailFrom            string
	ConsoleURL          string        // where the SuperAdmin web app lives (used in invitation e-mails)
	BackupHeartbeatFile string        // touched by scripts/backup.sh; the System page reads its age
	DBMaxConns          int           // pool size per API instance
	SlowQuery           time.Duration // statements slower than this are logged (SQL text only)
	RLS                 bool          // set app.society_id on tenant connections and run with row-level security
}

// IsDev enables dev-only behaviour (fixed OTP, console SMS, seed data).
func (c Config) IsDev() bool { return c.Env == "dev" || c.Env == "test" }

func Load() (Config, error) {
	env := get("APP_ENV", "dev")
	cfg := Config{
		Env:             env,
		HTTPAddr:        get("HTTP_ADDR", ":8080"),
		DatabaseURL:     get("DATABASE_URL", "postgres://nexora:nexora@localhost:5432/nexora?sslmode=disable"),
		RedisAddr:       get("REDIS_ADDR", "localhost:6379"),
		JWTSecret:       get("JWT_SECRET", ""),
		AccessTTL:       15 * time.Minute,
		RefreshTTL:      30 * 24 * time.Hour,
		OTPTTL:          5 * time.Minute,
		AutoMigrate:     getBool("AUTO_MIGRATE", env == "dev"),
		ShutdownTimeout: 15 * time.Second,
	}
	cfg.VisitorMaxWindow = 7 * 24 * time.Hour
	cfg.VisitorWalkinWait = 10 * time.Minute
	if v, err := strconv.Atoi(os.Getenv("VISITOR_MAX_WINDOW_DAYS")); err == nil && v > 0 {
		cfg.VisitorMaxWindow = time.Duration(v) * 24 * time.Hour
	}
	if v, err := strconv.Atoi(os.Getenv("VISITOR_WALKIN_TIMEOUT_MIN")); err == nil && v > 0 {
		cfg.VisitorWalkinWait = time.Duration(v) * time.Minute
	}
	cfg.SOSEscalateAfter, cfg.SOSCancelWindow, cfg.NotifyRetention = 60*time.Second, 2*time.Minute, 90*24*time.Hour
	if v, err := strconv.Atoi(os.Getenv("SOS_ESCALATE_AFTER_SEC")); err == nil && v > 0 {
		cfg.SOSEscalateAfter = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("SOS_CANCEL_WINDOW_SEC")); err == nil && v > 0 {
		cfg.SOSCancelWindow = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("NOTIFY_RETENTION_DAYS")); err == nil && v > 0 {
		cfg.NotifyRetention = time.Duration(v) * 24 * time.Hour
	}
	cfg.ComplaintAutoclose, cfg.ComplaintReopen, cfg.AttachmentMaxMB = 7*24*time.Hour, 7*24*time.Hour, 5
	if v, err := strconv.Atoi(os.Getenv("COMPLAINT_AUTOCLOSE_DAYS")); err == nil && v > 0 {
		cfg.ComplaintAutoclose = time.Duration(v) * 24 * time.Hour
	}
	if v, err := strconv.Atoi(os.Getenv("COMPLAINT_REOPEN_DAYS")); err == nil && v > 0 {
		cfg.ComplaintReopen = time.Duration(v) * 24 * time.Hour
	}
	if v, err := strconv.Atoi(os.Getenv("ATTACHMENT_MAX_MB")); err == nil && v > 0 {
		cfg.AttachmentMaxMB = v
	}
	cfg.TrustProxy = getBool("TRUST_PROXY", false)
	cfg.MaxBodyMB, cfg.SlowRequest = 16, time.Second
	if v, err := strconv.Atoi(os.Getenv("MAX_BODY_MB")); err == nil && v > 0 {
		cfg.MaxBodyMB = v
	}
	if v, err := strconv.Atoi(os.Getenv("SLOW_REQUEST_MS")); err == nil && v > 0 {
		cfg.SlowRequest = time.Duration(v) * time.Millisecond
	}
	cfg.RateIPPerMin, cfg.RateUserPerMin = 600, 900
	if v, err := strconv.Atoi(os.Getenv("RATE_IP_PER_MIN")); err == nil && v >= 0 {
		cfg.RateIPPerMin = v
	}
	if v, err := strconv.Atoi(os.Getenv("RATE_USER_PER_MIN")); err == nil && v >= 0 {
		cfg.RateUserPerMin = v
	}
	cfg.MetricsToken = os.Getenv("METRICS_TOKEN")
	cfg.SentryDSN = os.Getenv("SENTRY_DSN")
	cfg.SMTPHost, cfg.SMTPUser, cfg.SMTPPass = os.Getenv("SMTP_HOST"), os.Getenv("SMTP_USER"), os.Getenv("SMTP_PASS")
	cfg.SMTPPort, _ = strconv.Atoi(get("SMTP_PORT", "587"))
	cfg.MailFrom = get("MAIL_FROM", "Nexora <no-reply@nexora.local>")
	cfg.ConsoleURL = os.Getenv("CONSOLE_URL")
	cfg.RLS = getBool("DB_RLS", false)
	cfg.BackupHeartbeatFile = os.Getenv("BACKUP_HEARTBEAT_FILE")
	cfg.SlowQuery = 500 * time.Millisecond
	if v, err := strconv.Atoi(os.Getenv("SLOW_QUERY_MS")); err == nil && v >= 0 {
		cfg.SlowQuery = time.Duration(v) * time.Millisecond
	}
	cfg.DBMaxConns = 25
	if v, err := strconv.Atoi(os.Getenv("DB_MAX_CONNS")); err == nil && v > 0 {
		cfg.DBMaxConns = v
	}
	cfg.AmenityReminder = time.Hour
	if v, err := strconv.Atoi(os.Getenv("AMENITY_REMINDER_MIN")); err == nil && v > 0 {
		cfg.AmenityReminder = time.Duration(v) * time.Minute
	}
	cfg.PublicBaseURL = os.Getenv("PUBLIC_BASE_URL")
	cfg.PaymentFake = os.Getenv("PAYMENT_PROVIDER") == "fake" || env == "dev"
	cfg.FCMProjectID = os.Getenv("FCM_PROJECT_ID")
	cfg.FCMCredentialsFile = os.Getenv("FCM_CREDENTIALS_FILE")
	cfg.BlobPath = os.Getenv("BLOB_PATH")
	if cfg.BlobPath == "" && env == "dev" {
		cfg.BlobPath = "./.data/blobs"
	}
	cfg.PlatformJWTSecret = get("PLATFORM_JWT_SECRET", "")
	cfg.PlatformEncKey = get("PLATFORM_ENC_KEY", "")
	if v := os.Getenv("DEV_FIXED_MFA"); v != "" {
		// a known 2FA code would defeat the second factor, so it can never be switched on outside APP_ENV=dev
		if cfg.Env != "dev" {
			return cfg, fmt.Errorf("DEV_FIXED_MFA is only allowed when APP_ENV=dev")
		}
		if len(v) != 6 || strings.Trim(v, "0123456789") != "" {
			return cfg, fmt.Errorf("DEV_FIXED_MFA must be exactly 6 digits")
		}
		cfg.DevFixedMFA = v
	}
	cfg.PlatformAccessTTL = 15 * time.Minute
	cfg.PlatformRefreshTTL = 7 * 24 * time.Hour
	if v := os.Getenv("PLATFORM_ALLOWED_CIDRS"); v != "" {
		cfg.PlatformAllowedCIDRs = strings.Split(v, ",")
	}
	if cfg.IsDev() {
		if cfg.PlatformJWTSecret == "" {
			cfg.PlatformJWTSecret = "dev-platform-secret-dev-platform-secret"
		}
		if cfg.PlatformEncKey == "" {
			cfg.PlatformEncKey = "dev-platform-enc-key-not-for-production"
		}
	} else if len(cfg.PlatformJWTSecret) < 32 || len(cfg.PlatformEncKey) < 32 {
		return cfg, fmt.Errorf("PLATFORM_JWT_SECRET and PLATFORM_ENC_KEY (32+ chars) are required outside dev")
	}
	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		cfg.CORSOrigins = strings.Split(v, ",")
	} else if cfg.IsDev() {
		cfg.CORSOrigins = []string{"*"} // dev only; set CORS_ORIGINS in staging/prod
	}
	if cfg.JWTSecret == "" {
		if !cfg.IsDev() {
			return cfg, fmt.Errorf("JWT_SECRET is required outside dev")
		}
		cfg.JWTSecret = "dev-secret-change-me-dev-secret-change-me"
	}
	if len(cfg.JWTSecret) < 32 && !cfg.IsDev() {
		return cfg, fmt.Errorf("JWT_SECRET must be at least 32 chars")
	}
	return cfg, nil
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// Validate refuses unsafe settings outside dev/test: default secrets, fake payments, wildcard CORS and so on.
// It is called by the API process at start-up (tests build Config values directly).
func (c Config) Validate() error {
	if c.IsDev() {
		return nil
	}
	var bad []string
	add := func(s string) { bad = append(bad, s) }
	if strings.Contains(c.JWTSecret, "change-me") || strings.Contains(c.JWTSecret, "dev-") {
		add("JWT_SECRET looks like a development value")
	}
	if strings.HasPrefix(c.PlatformJWTSecret, "dev-") || strings.HasPrefix(c.PlatformEncKey, "dev-") {
		add("PLATFORM_JWT_SECRET / PLATFORM_ENC_KEY look like development values")
	}
	if c.JWTSecret == c.PlatformJWTSecret {
		add("JWT_SECRET and PLATFORM_JWT_SECRET must differ")
	}
	if c.PaymentFake {
		add("the fake payment gateway is on (unset PAYMENT_PROVIDER=fake)")
	}
	if len(c.CORSOrigins) == 0 {
		add("CORS_ORIGINS is empty")
	}
	for _, o := range c.CORSOrigins {
		if strings.TrimSpace(o) == "*" {
			add("CORS_ORIGINS must not contain *")
		}
	}
	if strings.Contains(c.DatabaseURL, "nexora:nexora@") {
		add("DATABASE_URL uses the default password")
	}
	if strings.Contains(c.DatabaseURL, "sslmode=disable") && !strings.Contains(c.DatabaseURL, "localhost") && !strings.Contains(c.DatabaseURL, "127.0.0.1") {
		add("DATABASE_URL has sslmode=disable for a remote database")
	}
	if !strings.HasPrefix(c.PublicBaseURL, "https://") {
		add("PUBLIC_BASE_URL must be an https:// address")
	}
	if c.RateIPPerMin == 0 {
		add("RATE_IP_PER_MIN=0 switches the per-IP limit off")
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("unsafe configuration for APP_ENV=%s:\n - %s", c.Env, strings.Join(bad, "\n - "))
}
