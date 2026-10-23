DB_URL ?= postgres://nexora:nexora@localhost:5432/nexora?sslmode=disable

.PHONY: up down run build test lint migrate-up migrate-down
up:        ; docker compose -f deployments/docker-compose.yml up -d
down:      ; docker compose -f deployments/docker-compose.yml down
run:       ; go run ./cmd/api
build:     ; go build -o bin/api ./cmd/api
test:      ; go test ./... -race
lint:      ; golangci-lint run
migrate-up:   ; migrate -path db/migrations -database "$(DB_URL)" up
migrate-down: ; migrate -path db/migrations -database "$(DB_URL)" down 1
