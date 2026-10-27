DB_URL ?= postgres://nexora:nexora@localhost:5432/nexora?sslmode=disable

.PHONY: dev up down run build test test-rls test-pool1 test-load test-short lint migrate-up migrate-down seed
totp:         ; go run ./cmd/totp   # current 2FA codes for the seeded console staff
dev:          ; go run ./cmd/devserver   # no Docker: embedded Postgres + in-memory Redis + seed
up:           ; docker compose -f deployments/docker-compose.yml up -d
down:         ; docker compose -f deployments/docker-compose.yml down
run:          ; go run ./cmd/api
build:        ; go build -o bin/api ./cmd/api
test:         ; go test ./... -race
test-rls:     ; TEST_RLS=1 go test ./internal/app/... -race   # the whole suite as the row-level-security role
test-pool1:   ; TEST_MAXCONNS=1 go test ./internal/app/... -timeout 6m   # one-connection pool: any code holding a connection while asking for another hangs
test-load:    ; go run ./cmd/loadtest -vus 200 -dur 30s -out docs/perf/latest.md
test-short:   ; go test ./... -short
lint:         ; golangci-lint run
migrate-up:   ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up
migrate-down: ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate down
seed:         ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate seed
