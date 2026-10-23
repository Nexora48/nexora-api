package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	Env             string
	HTTPAddr        string
	DatabaseURL     string
	RedisAddr       string
	JWTSecret       string
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Env:             get("APP_ENV", "dev"),
		HTTPAddr:        get("HTTP_ADDR", ":8080"),
		DatabaseURL:     get("DATABASE_URL", "postgres://nexora:nexora@localhost:5432/nexora?sslmode=disable"),
		RedisAddr:       get("REDIS_ADDR", "localhost:6379"),
		JWTSecret:       get("JWT_SECRET", ""),
		ShutdownTimeout: 15 * time.Second,
	}
	if cfg.Env == "prod" && cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("JWT_SECRET is required in prod")
	}
	return cfg, nil
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
