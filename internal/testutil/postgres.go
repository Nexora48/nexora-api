// Package testutil starts throwaway Postgres/Redis for integration tests (no Docker needed).
package testutil

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// StartPostgres boots an embedded Postgres 16 and returns its URL and a stop func.
func StartPostgres() (url string, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	dir, err := os.MkdirTemp("", "nexora-pg-*")
	if err != nil {
		return "", nil, err
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		Username("nexora").Password("nexora").Database("nexora").
		RuntimePath(filepath.Join(dir, "rt")).
		DataPath(filepath.Join(dir, "data")))
	if err := pg.Start(); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	url = fmt.Sprintf("postgres://nexora:nexora@127.0.0.1:%d/nexora?sslmode=disable", port)
	return url, func() { _ = pg.Stop(); os.RemoveAll(dir) }, nil
}

// StartPostgresAt is StartPostgres with a data directory that outlives the process: the same
// folder and port give the same database next time (used by the dev server to keep your data).
// stop shuts Postgres down and leaves the data in place.
func StartPostgresAt(dir string, port uint32) (url string, stop func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		Username("nexora").Password("nexora").Database("nexora").
		RuntimePath(filepath.Join(dir, "rt")).
		DataPath(filepath.Join(dir, "data")))
	if err := pg.Start(); err != nil {
		return "", nil, err
	}
	url = fmt.Sprintf("postgres://nexora:nexora@127.0.0.1:%d/nexora?sslmode=disable", port)
	return url, func() { _ = pg.Stop() }, nil
}
