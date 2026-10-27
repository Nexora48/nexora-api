# API down or returning errors

1. **Is it up?** `curl -s https://api.example.com/health` (liveness) and `/ready` (database and Redis). Check the process/container: restarts, OOM kills, exit code.
2. **Startup refuses?** The log lists what is unsafe (`unsafe configuration for APP_ENV=...`). Fix the environment variables it names.
3. **5xx spike?** Grafana → API overview → *5xx ratio* and *slowest routes*. In Sentry open the newest issue: tags show route, request id and society id (no personal data). Search the log by that request id.
4. **After a release?** Roll back (`deploy-rollback.md`).
5. **Database down?** `docs/runbooks/backup-restore.md`; the API returns 5xx until it is back. **Redis down?** `redis.md` (sign-in refuses with 503 by design; everything else keeps working).
6. **Background job stalled** (alert `BackgroundJobStalled`)? The System page lists each job and when it last ran. Restarting the API restarts all jobs; they are idempotent.
7. **Rate-limit surge?** Look at `nexora_rate_limited_total` by limiter. One office NAT can trip the per-IP limit: raise `RATE_IP_PER_MIN`. A single abusive IP: block it at the proxy.
8. Tell the societies if it lasts more than a few minutes: SuperAdmin → Staff → Broadcast reaches every society's admins.
