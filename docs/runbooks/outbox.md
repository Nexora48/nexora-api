# Push or SMS not arriving

1. System page → *Message outbox*: waiting, oldest age, failed for good. Same numbers as `nexora_outbox_pending`, `nexora_outbox_oldest_age_seconds`, `nexora_outbox_failed`.
2. **Waiting and growing, oldest old:** the worker is not sending. Check the `notify-outbox` job in System → background jobs; check FCM credentials and reachability from the server (`FCM_PROJECT_ID`, `FCM_CREDENTIALS_FILE`). The worker retries with back-off up to 5 attempts.
3. **Failed for good:** `SELECT last_error, count(*) FROM notification_outbox WHERE sent_at IS NULL AND attempts >= 5 GROUP BY 1;` Typical causes: expired device tokens (harmless: the app registers a new one at next start), wrong FCM credentials (fix and re-queue: `UPDATE notification_outbox SET attempts=0, next_at=now() WHERE sent_at IS NULL AND attempts >= 5 AND created_at > now() - interval '1 day';`).
4. In-app notifications and live updates do **not** depend on the outbox; only push does. If push is down for long, tell societies via a Broadcast (it also lands in the inbox).
5. OTP/SMS has its own sender (`SMS` provider); an SMS outage stops new sign-ins by OTP. Residents already signed in are unaffected.
