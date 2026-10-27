# Go-live checklist

Tick every line before real residents use the system. Nothing here is automated.

## Environment
- [ ] `APP_ENV=prod` (or `staging`); the API starts (it refuses unsafe config)
- [ ] `JWT_SECRET`, `PLATFORM_JWT_SECRET`, `PLATFORM_ENC_KEY` are random, 32+ characters, all different, stored in a secret manager, not in git
- [ ] `DATABASE_URL` uses a real password, `sslmode=require` (or verify-full) to a remote database
- [ ] `PAYMENT_PROVIDER` is **not** `fake`
- [ ] `CORS_ORIGINS` lists only the real web origins; `PUBLIC_BASE_URL` is the https address phones use
- [ ] `TRUST_PROXY=true` only if a proxy you control sets `X-Forwarded-For`, and it overwrites client-supplied values
- [ ] `PLATFORM_ALLOWED_CIDRS` limits the console to office/VPN ranges
- [ ] `RATE_IP_PER_MIN` and `RATE_USER_PER_MIN` set (defaults 600/900); behind a shared office NAT consider raising the IP value
- [ ] `DB_MAX_CONNS` x number of API instances is below Postgres `max_connections` with room for backups and exporters
- [ ] `DB_RLS=true` after `docs/runbooks/rls.md` was followed once on staging
- [ ] `SMTP_*` and `CONSOLE_URL` set so staff invitations are emailed; `MAIL_FROM` is a real sender
- [ ] FCM credentials set (push); SMS provider wired (OTP)
- [ ] seed data is off (`APP_ENV` is not `dev`); the first SuperAdmin was created with `go run ./cmd/superadmin`

## Network
- [ ] TLS terminates in front of the API; HTTP redirects to HTTPS; HSTS set at the proxy
- [ ] Postgres and Redis are not reachable from the internet
- [ ] `/metrics` is reachable only from Prometheus (token set)

## Data safety
- [ ] `scripts/backup.sh` runs daily from cron or a scheduler; `BACKUP_HEARTBEAT_FILE` is set and visible in the System page
- [ ] backups are copied off the machine (`BACKUP_S3_URI` or your own copy job)
- [ ] a **restore drill** passed in the last month (`scripts/restore-drill.sh`)
- [ ] WAL archiving / PITR configured if you need to recover to a minute (`deployments/docker-compose.pitr.yml` shows how)
- [ ] blob store backed up; `go run ./cmd/blobcheck` reports no missing files

## Observability
- [ ] Prometheus scrapes the API; Grafana dashboards open; alert rules loaded
- [ ] alert delivery (email/Slack) replaced the placeholder in `alertmanager.yml` and a test alert arrived
- [ ] `SENTRY_DSN` set and a deliberate error appeared in Sentry (without personal data)
- [ ] someone knows where the runbooks are and who is on call

## Apps
- [ ] app builds use the production `API_BASE_URL`; store links entered in SuperAdmin → System → App versions
- [ ] a pilot society was onboarded in SuperAdmin with a plan; its admin changed the temporary password
