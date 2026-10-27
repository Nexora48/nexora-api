# Rotating and leaking secrets

Rule of thumb: **assume a leaked secret is already used**. Rotate first, investigate second.

## JWT_SECRET (residents, guards, office)
1. Generate 48+ random characters. 2. Replace the value in the secret manager and restart **all** instances at once. 3. Everyone is signed out and signs in again (OTP / password). 4. Review the audit log for the leak window (SuperAdmin → Audit).

## PLATFORM_JWT_SECRET (console)
Same steps. Staff sign in again with their authenticator. If the IP allow-list was off, also reset staff authenticators (Staff → … → Reset authenticator) and review audit entries by staff.

## PLATFORM_ENC_KEY (encrypts gateway keys and staff TOTP secrets)
It cannot be changed in place: a new key cannot read old ciphertext. Options:
- **Planned rotation:** write a one-off script that reads each `payment_gateway_settings` and `platform_users.totp_secret_enc` value with the old key and writes it back with the new one (run it while the API is stopped); then start with the new key.
- **After a leak:** ask each society to re-enter its Razorpay keys (and rotate them in Razorpay first), reset all staff authenticators, then switch the key.
Keep the key in the secret manager with the database backups' retention: a backup without the key cannot decrypt those columns.

## Society Razorpay keys
Rotate in the Razorpay dashboard, enter the new key and secret in Admin → Payments → Gateway (write-only), use "Test connection". Update the webhook secret in both places.

## METRICS_TOKEN, SENTRY_DSN, SMTP password, FCM credentials
Rotate at the provider, update the environment, restart. Prometheus reads the token from `deployments/monitoring/api-token`.

## Database password
`ALTER ROLE nexora PASSWORD '...'`, update `DATABASE_URL` everywhere (API, backup jobs, exporters), restart.

## After any rotation
Check the System page (all green), sign in on each app, run `scripts/backup.sh` once to prove the backup job still connects.
