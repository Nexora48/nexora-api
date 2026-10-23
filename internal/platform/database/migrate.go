package database

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func newMigrator(url string, fsys fs.FS) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, "migrations")
	if err != nil {
		return nil, err
	}
	u := strings.Replace(url, "postgresql://", "pgx5://", 1)
	u = strings.Replace(u, "postgres://", "pgx5://", 1)
	return migrate.NewWithSourceInstance("iofs", src, u)
}

// MigrateUp applies all pending migrations.
func MigrateUp(url string, fsys fs.FS) error {
	m, err := newMigrator(url, fsys)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// MigrateDown rolls back one migration.
func MigrateDown(url string, fsys fs.FS) error {
	m, err := newMigrator(url, fsys)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Steps(-1)
}
