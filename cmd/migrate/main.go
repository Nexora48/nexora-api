// Command migrate runs DB migrations and dev seeding: `go run ./cmd/migrate up|down|seed`.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Nexora48/nexora-api/db"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/seed"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: migrate up|down|seed")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "up":
		fail(database.MigrateUp(cfg.DatabaseURL, db.Migrations))
	case "down":
		fail(database.MigrateDown(cfg.DatabaseURL, db.Migrations))
	case "seed":
		if !cfg.IsDev() {
			fail(fmt.Errorf("seed is dev-only (APP_ENV=%s)", cfg.Env))
		}
		ctx := context.Background()
		pool, err := database.NewPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			fail(err)
		}
		defer pool.Close()
		fail(seed.Dev(ctx, pool))
		fmt.Println("seeded: admin@sunrise.test / Admin@123, resident phone 9999999999 (OTP 123456)")
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
