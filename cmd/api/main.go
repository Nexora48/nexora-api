package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/health"
	"github.com/Nexora48/nexora-api/internal/middleware"
	"github.com/Nexora48/nexora-api/internal/platform/cache"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/logger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	log := logger.New(cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checks := map[string]health.Pinger{}

	// Dependencies are optional in dev so `go run` works without Docker; /ready reports them.
	if pool, err := database.NewPostgres(ctx, cfg.DatabaseURL); err != nil {
		log.Warn("postgres unavailable", "err", err)
		checks["postgres"] = func(context.Context) error { return err }
	} else {
		defer pool.Close()
		checks["postgres"] = pool.Ping
	}
	if rdb, err := cache.NewRedis(ctx, cfg.RedisAddr); err != nil {
		log.Warn("redis unavailable", "err", err)
		checks["redis"] = func(context.Context) error { return err }
	} else {
		defer rdb.Close()
		checks["redis"] = func(c context.Context) error { return rdb.Ping(c).Err() }
	}

	h := health.New(checks)
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, middleware.Recoverer(log), middleware.Logger(log))
	r.Get("/health", h.Live)
	r.Get("/ready", h.Ready)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/ping", h.Live) // modules mount here from M1 onward
	})

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: r}
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(sctx)
	log.Info("stopped")
}
