package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env             string
	HTTPAddr        string
	DatabaseURL     string
	RedisAddr       string
	JWTSecret       string
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	OTPTTL          time.Duration
	AutoMigrate     bool
	CORSOrigins     []string
	ShutdownTimeout time.Duration
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
