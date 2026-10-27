# Security notes

How the Nexora API protects data, what was reviewed in M11, and what to do when a secret leaks.
Report a vulnerability privately to the maintainers; do not open a public issue.

## Layers (outermost first)
1. **Edge:** global per-IP rate limit (`RATE_IP_PER_MIN`), body ceiling (`MAX_BODY_MB`), server timeouts, security headers (`nosniff`, `X-Frame-Options: DENY`, `no-store` on `/api`). `X-Forwarded-For` is trusted **only** when `TRUST_PROXY=true`.
2. **Authentication:** OTP (residents, guards) and password + authenticator (office, staff). Access tokens live 15 minutes, HS256 pinned, audience and issuer checked; refresh tokens rotate and a replay revokes the family. Sign-in, OTP and MFA limiters **fail closed** when Redis is down.
3. **Authorisation:** permission per route (RBAC), service switch per society (`FEATURE_DISABLED`), plan limits (`PLAN_LIMIT`), and the society always comes from the token, never from the request.
4. **Tenant isolation:** every query filters `society_id` **and** PostgreSQL row-level security (migration 51, role `nexora_app`, `DB_RLS=true`) refuses any row of another society even if a query forgets the filter. Code with no signed-in user (sign-in, sweepers, webhooks, the console) stays on the table-owner login role, which the policies do not apply to; the one deliberate cross-society read per signed-in user uses `database.Unscoped` (two places, listed in `docs/runbooks/rls.md`).
5. **Data:** secrets encrypted with AES-GCM (`PLATFORM_ENC_KEY`), passwords hashed with Argon2id, gateway keys write-only, no secret in any response, log, audit row or Sentry event (tests scan for this).

## M11 review (OWASP API Top 10 walk-through)
| Area | Result |
|---|---|
| Broken object-level authorisation | RLS plus a test that walks every route with another society's real ids as admin, resident and guard: nothing answers 2xx |
| Broken authentication | token lifetimes, rotation and reuse detection verified; sign-in limiters fail closed (was fail-open: **fixed**) |
| Broken property-level authorisation / mass assignment | strict JSON decoding rejects unknown fields |
| Unrestricted resource consumption | global, per-user and per-route limits; body ceiling; page-size caps; statement timeout 30 s; pool size configurable (`DB_MAX_CONNS`) |
| Function-level authorisation | permission middleware on every group; `platform_support` is read-only |
| Server-side request forgery | the only outbound hosts are fixed: Razorpay, FCM and the SMTP relay |
| Security misconfiguration | `config.Validate` refuses default secrets, fake payments, wildcard CORS, missing https in production; **`X-Forwarded-For` was trusted from anyone: fixed** (`TRUST_PROXY`) |
| Improper inventory | OpenAPI 1.2.0 lists every route; `/metrics` needs a token or loopback |
| Unsafe consumption of APIs | webhooks are signature-verified, de-duplicated, amount-checked |
| Injection | every value is a bound parameter; the few formatted SQL fragments contain only numbers, constants or internal enums (reviewed, listed in the M11 outcome) |

Other findings fixed in M11: Go standard-library vulnerabilities (toolchain pinned to go1.26.10, `govulncheck` clean); **database pool deadlocks** (three code paths held a connection while asking for a second; a one-connection test run now guards this); plan-limit check could be exceeded by parallel creations (per-society lock); unknown-order webhook path held a transaction while writing.

Open, accepted: no CSP on the hosted payment page (Razorpay needs its own script and frames; revisit when the hosted page is redesigned); JWT secrets rotate by replacing the value and signing everyone out (no `kid` overlap); audit rows are not hash-chained.

## Secret rotation and leaks
| Secret | If it leaks | Rotate by |
|---|---|---|
| `JWT_SECRET` | anyone can mint resident/office tokens | set a new value and restart: every session ends, people sign in again |
| `PLATFORM_JWT_SECRET` | anyone can mint console tokens | same; also reset staff TOTP if the IP allow-list is off |
| `PLATFORM_ENC_KEY` | stored gateway keys and TOTP secrets are readable with DB access | **cannot be swapped in place**: re-encrypt first (decrypt with the old key, write with the new) or have each society re-enter its gateway keys and staff re-enrol |
| Society Razorpay keys | funds are not held by Nexora, but orders can be created | the society rotates them in Razorpay and re-enters them in Admin → Payments |
| `METRICS_TOKEN` | metrics readable | new value in the API and in Prometheus `api-token` |
| Database password | full data access | change in Postgres, update `DATABASE_URL`, restart |
| `SENTRY_DSN` | someone can send junk events | rotate the DSN in Sentry |
See `docs/runbooks/secrets.md` for the step-by-step.
