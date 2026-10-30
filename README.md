# nexora-api
Go backend (modular monolith) for the Nexora residential platform. See `../DEVELOPMENT_PLAN.md`, `../MILESTONES.md`, `../SKILL.md`.

## Run without Docker (recommended for app development)
```bash
make dev        # embedded Postgres + in-memory Redis + migrations + seed  → http://localhost:8080
                # data lives in .data/pg and survives restarts; `make dev-fresh` wipes it and re-seeds
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

## SuperAdmin console API
Dev staff (seeded by `make dev`): `super@nexora.test` / `Super@12345` (super_admin) and `support@nexora.test` / `Support@12345` (read-only).
In `make dev` every staff 2FA prompt also accepts `123456` (set `DEV_FIXED_MFA` to change it); the API refuses that setting unless `APP_ENV=dev`.
Two-factor codes: `make totp` prints the current code for each.
Create real staff: `go run ./cmd/superadmin create-user -email a@nexora.com -name "A B" -role super_admin`; lost authenticator: `go run ./cmd/superadmin reset-mfa -email …`.
Config: `PLATFORM_JWT_SECRET`, `PLATFORM_ENC_KEY` (both 32+ chars, required outside dev), optional `PLATFORM_ALLOWED_CIDRS`.
