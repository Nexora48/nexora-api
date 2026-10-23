// Package app wires every module into one chi router (used by main and tests).
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/health"
	"github.com/Nexora48/nexora-api/internal/middleware"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/rbac"
	"github.com/Nexora48/nexora-api/internal/society"
	"github.com/Nexora48/nexora-api/internal/user"
)

type Deps struct {
	Cfg   config.Config
	Log   *slog.Logger
	Pool  *pgxpool.Pool
	Redis *redis.Client
	SMS   sms.Sender
}

// App exposes the router plus the middleware factory modules use to guard routes.
type App struct {
	Router *chi.Mux
	// Protect returns the standard chain: auth → tenant → permission? → feature?.
	// Empty perm / feature skip that check.
	Protect func(perm, featureKey string) chi.Middlewares
}

func New(d Deps) *App {
	tokens := auth.NewTokenManager(d.Cfg.JWTSecret, d.Cfg.AccessTTL)
	users := user.NewRepository(d.Pool)
	societies := society.NewRepository(d.Pool)
	rb := rbac.New(d.Pool)
	feats := feature.New(d.Pool)
	aud := audit.New(d.Pool)
	otp := auth.NewOTPService(d.Redis, d.SMS, d.Cfg.JWTSecret, d.Cfg.OTPTTL, d.Cfg.IsDev())
	authSvc := auth.NewService(auth.NewRepository(d.Pool), users, societies, tokens, otp, d.Redis, aud, d.Cfg.RefreshTTL)
	authH := auth.NewHandler(authSvc)
	meH := user.NewHandler(users, societies, rb, feats)

	protect := func(perm, featureKey string) chi.Middlewares {
		chain := chi.Middlewares{middleware.Auth(tokens), middleware.Tenant(societies)}
		if perm != "" {
			chain = append(chain, middleware.RequirePermission(rb, perm))
		}
		if featureKey != "" {
			chain = append(chain, middleware.RequireFeature(feats, featureKey))
		}
		return chain
	}

	hl := health.New(map[string]health.Pinger{
		"postgres": d.Pool.Ping,
		"redis":    func(c context.Context) error { return d.Redis.Ping(c).Err() },
	})

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, middleware.Recoverer(d.Log), middleware.Logger(d.Log))
	r.Get("/health", hl.Live)
	r.Get("/ready", hl.Ready)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/ping", hl.Live)

		r.Route("/auth", func(r chi.Router) {
			r.With(middleware.RateLimit(d.Redis, "otp", 20, time.Hour)).Post("/otp/request", authH.RequestOTP)
			r.With(middleware.RateLimit(d.Redis, "verify", 60, 10*time.Minute)).Post("/otp/verify", authH.VerifyOTP)
			r.With(middleware.RateLimit(d.Redis, "login", 60, 10*time.Minute)).Post("/login", authH.PasswordLogin)
			r.Post("/select-society", authH.SelectSociety)
			r.Post("/refresh", authH.Refresh)
			r.With(middleware.Auth(tokens)).Post("/logout", authH.Logout)
		})

		r.Route("/me", func(r chi.Router) {
			r.Use(protect("", "")...)
			r.Get("/", meH.Me)
			r.Get("/features", meH.Features)
			r.Put("/devices", meH.RegisterDevice)
		})
	})
	return &App{Router: r, Protect: protect}
}
