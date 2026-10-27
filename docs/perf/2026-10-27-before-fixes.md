# Load test

- date: 2026-10-27 21:56
- hardware: darwin/arm64, 10 CPUs, go1.26.0
- flats: 500, residents: 210, guards: 20, historical visits: 20000, notifications: 50000
- virtual users: 300 for 30s

## Single-shot timings

| step | time |
|---|---|
| generate invoices for 500 flats (draft) | 164ms |
| issue invoices for 500 flats | 768ms |
| collection report | 18ms |
| dues and ageing | 14ms |
| invoices CSV export | 168ms |
| visitors CSV export (all history) | 118ms |
| visitor stats | 17ms |
| SOS reaches 21 connected guards and office (raise returned 201) | 12ms |

## Steady load (300 virtual users)

| call | n | p50 | p95 | p99 | max | 5xx/errors |
|---|---|---|---|---|---|---|
| guard: dashboard | 5 | 1m0.004s | 1m0.006s ⚠ | 1m0.006s | 1m0.006s | 5 |
| guard: flat lookup | 6 | 1m0.004s | 1m0.005s ⚠ | 1m0.005s | 1m0.006s | 4 |
| guard: inside now | 5 | 1m0s | 1m0.003s ⚠ | 1m0.003s | 1m0.005s | 3 |
| guard: passes sync | 4 | 1m0.003s | 1m0.004s ⚠ | 1m0.004s | 1m0.006s | 4 |
| guard: verify a pass | 7 | 1m0.004s | 1m0.005s ⚠ | 1m0.005s | 1m0.005s | 6 |
| guard: visits | 9 | 1m0.002s | 1m0.004s ⚠ | 1m0.004s | 1m0.006s | 5 |
| office: complaints | 11 | 161ms | 1m0s ⚠ | 1m0s | 1m0.003s | 2 |
| office: dues | 11 | 1m0.005s | 1m0.007s ⚠ | 1m0.007s | 1m0.008s | 11 |
| office: invoices list | 12 | 1m0.005s | 1m0.006s ⚠ | 1m0.006s | 1m0.007s | 11 |
| office: residents list | 11 | 59.941s | 1m0.005s ⚠ | 1m0.005s | 1m0.005s | 5 |
| office: stats | 8 | 82ms | 1m0.002s ⚠ | 1m0.002s | 1m0.005s | 2 |
| office: units list | 5 | 1m0.002s | 1m0.003s ⚠ | 1m0.003s | 1m0.006s | 3 |
| office: visitor stats | 11 | 59.981s | 1m0.005s ⚠ | 1m0.005s | 1m0.005s | 5 |
| office: visitors list | 15 | 1m0s | 1m0.005s ⚠ | 1m0.005s | 1m0.005s | 8 |
| resident: bills | 26 | 1m0.002s | 1m0.007s ⚠ | 1m0.008s | 1m0.008s | 26 |
| resident: features | 22 | 68ms | 1m0.005s ⚠ | 1m0.006s | 1m0.006s | 10 |
| resident: ledger | 29 | 1m0.001s | 1m0.007s ⚠ | 1m0.007s | 1m0.007s | 27 |
| resident: me | 31 | 1m0.003s | 1m0.008s ⚠ | 1m0.008s | 1m0.008s | 31 |
| resident: my visitors | 21 | 1m0.001s | 1m0.007s ⚠ | 1m0.007s | 1m0.008s | 19 |
| resident: notices | 35 | 1m0.002s | 1m0.007s ⚠ | 1m0.008s | 1m0.008s | 32 |
| resident: notifications | 32 | 59.863s | 1m0.003s ⚠ | 1m0.004s | 1m0.005s | 12 |
| resident: pre-approve visitor | 27 | 1m0.007s | 1m0.008s ⚠ | 1m0.008s | 1m0.008s | 27 |
| resident: unread count | 30 | 59.899s | 1m0.003s ⚠ | 1m0.004s | 1m0.006s | 14 |

Total 373 requests, 272 errors (5xx or network).

Heap after GC: 5 MB before, 8 MB after (goroutines 92).
