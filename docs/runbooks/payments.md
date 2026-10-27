# Payments and webhooks

Money rules: integer paise, one payment per order, events de-duplicated, only a signature-verified webhook (or the test gateway in dev) marks an order paid.

## A resident paid but the bill is open
1. Admin → Payments → Gateway: is the society's Razorpay configured and active? Is the webhook URL (shown there) registered in the Razorpay dashboard with the same secret?
2. System page → *Payment webhooks* (also `nexora_payment_events_stuck`): events received but not processed for 5+ minutes.
3. In Postgres: `SELECT event_id, processed, note, received_at FROM payment_events WHERE society_id='<id>' ORDER BY received_at DESC LIMIT 20;` and `SELECT status, note FROM payment_orders WHERE id='<order>';` A note `amount mismatch` means the gateway amount differs from the order: the office was alerted, nothing was applied; settle manually (Admin → record payment) after checking the Razorpay dashboard.
4. **Replay is safe:** re-delivering an event from the Razorpay dashboard cannot pay twice (events are keyed by id, orders are locked, `payments.order_id` is unique).
5. A payment that never reached the webhook: record it in Admin → Payments → Record payment with the gateway reference; do not edit rows by hand.

## Webhook failures spike
`nexora_payment_webhooks_total{outcome="rejected"}` rising means signatures fail: wrong webhook secret for that society, or someone probing the URL. 401 responses are expected for the latter.

## Smoke test with real test keys
`RAZORPAY_KEY_ID=rzp_test_... RAZORPAY_KEY_SECRET=... RAZORPAY_WEBHOOK_SECRET=... go run ./cmd/razorpay-check` (refuses live keys).
