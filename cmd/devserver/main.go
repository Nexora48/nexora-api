// Command devserver runs the whole backend with NO external services:
// embedded Postgres + in-memory Redis + migrations + dev seed. Data is thrown away on exit.
//
//	go run ./cmd/devserver            # listens on :8080
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/db"
	"github.com/Nexora48/nexora-api/internal/app"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/seed"
	"github.com/Nexora48/nexora-api/internal/testutil"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting embedded postgres (first run downloads binaries)...")
	url, stopPG, err := testutil.StartPostgres()
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer stopPG()
	if err := database.MigrateUp(url, db.Migrations); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := database.NewPostgres(ctx, url)
	if err != nil {
		log.Error("pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := seed.Dev(ctx, pool); err != nil {
		log.Error("seed", "err", err)
		os.Exit(1)
	}
	if err := seed.Demo(ctx, pool); err != nil {
		log.Error("seed demo", "err", err)
		os.Exit(1)
	}
	cfg0, _ := config.Load()
	if err := seed.Platform(ctx, pool, cfg0.PlatformEncKey); err != nil {
		log.Error("seed platform", "err", err)
		os.Exit(1)
	}
	mr, err := miniredis.Run()
	if err != nil {
		log.Error("redis", "err", err)
		os.Exit(1)
	}
	defer mr.Close()

	cfg, _ := config.Load()
	a := app.New(app.Deps{Cfg: cfg, Log: log, Pool: pool,
		Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), SMS: sms.ConsoleSender{Log: log}})
	go a.Visitors.RunSweeper(ctx, time.Minute)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Router}
	go func() {
		log.Info("devserver ready", "addr", cfg.HTTPAddr,
			"admin", seed.AdminEmail+" / "+seed.AdminPass, "resident_phone", seed.ResidentPhone, "otp", "123456")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}
