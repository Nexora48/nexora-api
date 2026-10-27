# Redis lost or unreachable

Redis holds only short-lived state: OTP codes and cool-downs, rate-limit counters, the realtime bridge between API instances, and the plan-limit creation locks. **Nothing in it needs a backup.**

## What happens when it goes away
| Part | Behaviour |
|---|---|
| Sign-in, OTP and MFA | **refuse with 503** (the limiters fail closed, so an attacker cannot use an outage to guess codes) |
| Per-IP / per-user limits | fail **open** and log a warning; traffic keeps flowing without limits |
| Plan-limit creation lock | falls back to the unlocked check (a tiny race can exceed a limit by one) |
| Live updates between instances | stop until it returns (one instance still serves its own connections) |
| Everyone already signed in | keeps working until the 15-minute access token ends; refresh tokens live in PostgreSQL |

## Recovery
Restart or replace Redis; nothing to restore. People who were mid-OTP request a new code. Confirm `/ready` and the `RedisDown` alert clearing. After a flush the API needs no restart (`TestRedisOutageFailsOpenForGeneralLimitsAndClosedForSignIn` covers the outage; the OTP tests run after `FlushAll`).
