# Slow responses or "pool exhausted"

Symptoms: p95 latency alert, `nexora_db_pool_connections{state="busy"}` near `max`, requests that wait or time out.

1. **Look at the pool:** Grafana → API overview → *Database pool*. Busy at max for minutes means requests are waiting for a connection.
2. **Look at Postgres:** `SELECT pid, state, wait_event_type, now()-query_start AS age, left(query,120) FROM pg_stat_activity WHERE datname='nexora' ORDER BY age DESC LIMIT 20;` Long `idle in transaction` sessions are killed after 60 s by the server setting; long `active` ones are killed after 30 s (`statement_timeout`).
3. **Slow statements:** the API logs `slow query` with the SQL (never the arguments). Threshold `SLOW_QUERY_MS` (default 500, 0 = off); the load test prints the worst ones with their totals.
4. **Raise the pool only if Postgres has room:** `DB_MAX_CONNS` (default 25) x instances must stay under `max_connections` minus headroom (backups, exporters, you). More connections rarely make Postgres faster.
5. **A new deadlock?** The classic cause is code that holds a connection (open rows or a transaction) while asking the pool for another: with enough concurrent requests every connection is held by someone waiting for one more. `make test-pool1` runs the whole test suite through a one-connection pool and hangs on exactly this mistake; `go run ./cmd/loadtest -probe` checks request kinds one by one. Fix: compute whatever needs another connection **before** the query/transaction starts, or finish with the rows first.
6. **Load test after a fix:** `make test-load` and compare with `docs/perf/`.
