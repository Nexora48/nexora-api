# nexora-api
Go backend (modular monolith) for the Nexora residential platform. See `../DEVELOPMENT_PLAN.md`, `../MILESTONES.md`, `../SKILL.md`.

## Run without Docker (recommended for app development)
```bash
make dev        # embedded Postgres + in-memory Redis + migrations + seed  → http://localhost:8080
```
Seed logins: admin `admin@sunrise.test` / `Admin@123`; resident phone `9999999999`, OTP `123456` (dev only).

## Run with Docker services
```bash
make up && make migrate-up && make seed && make run
```

## Tests
```bash
make test         # unit + integration (embedded Postgres, downloaded on first run)
make test-short   # unit only
```
