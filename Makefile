DB_URL ?= postgres://nexora:nexora@localhost:5432/nexora?sslmode=disable

.PHONY: dev up down run build test test-short lint migrate-up migrate-down seed
dev:          ; go run ./cmd/devserver   # no Docker: embedded Postgres + in-memory Redis + seed
up:           ; docker compose -f deployments/docker-compose.yml up -d
down:         ; docker compose -f deployments/docker-compose.yml down
run:          ; go run ./cmd/api
build:        ; go build -o bin/api ./cmd/api
test:         ; go test ./... -race
test-short:   ; go test ./... -short
lint:         ; golangci-lint run
migrate-up:   ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up
migrate-down: ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate down
seed:         ; DATABASE_URL="$(DB_URL)" go run ./cmd/migrate seed
