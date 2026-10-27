package main

import (
	"context"
	"errors"
	"github.com/Nexora48/nexora-api/internal/platform/errtrack"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Nexora48/nexora-api/db"
	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/platform/cache"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/logger"
	"github.com/Nexora48/nexora-api/internal/seed"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	log := logger.New(cfg.Env)
	flush, err := errtrack.Init(cfg.SentryDSN, cfg.Env, app.APIVersion)
	if err != nil {
		log.Warn("sentry disabled", "err", err)
	}
	defer flush()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.AutoMigrate {
		if err := database.MigrateUp(cfg.DatabaseURL, db.Migrations); err != nil {
			log.Error("migrate", "err", err)
			os.Exit(1)
		}
	}
	pool, err := database.NewPostgres(ctx, cfg.DatabaseURL, database.Options{RLS: cfg.RLS})
	if err != nil {
		log.Error("postgres unavailable (run `make up` or set DATABASE_URL)", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	rdb, err := cache.NewRedis(ctx, cfg.RedisAddr)
	if err != nil {
		log.Error("redis unavailable (run `make up` or set REDIS_ADDR)", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()

	if cfg.IsDev() {
		if err := seed.Dev(ctx, pool); err != nil {
			log.Warn("dev seed failed", "err", err)
		} else if err := seed.Demo(ctx, pool); err != nil {
			log.Warn("demo seed failed", "err", err)
		} else if err := seed.Platform(ctx, pool, cfg.PlatformEncKey); err != nil {
			log.Warn("platform seed failed", "err", err)
		}
	}

	a := app.New(app.Deps{Cfg: cfg, Log: log, Pool: pool, Redis: rdb, SMS: sms.ConsoleSender{Log: log}})
	a.Start(ctx)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Router,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
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
