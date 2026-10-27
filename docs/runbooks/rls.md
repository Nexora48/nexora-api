# Row-level security (RLS)

A second lock behind the `society_id` filters: PostgreSQL itself hides other societies' rows from the API's role.

## How it works
- Migration 51 creates the role `nexora_app` (no login, no bypass) and a policy on every table that has `society_id`: a row is visible and writable only when `society_id = app.society_id` (a plain equality, so the society indexes are still used). Child tables without `society_id` (invoice lines, poll options, ...) follow their parent. Platform-only tables (`platform_invoices`, `usage_daily`, ...) have row security on and no policy, so tenants see nothing.
- The tables are **not forced**: the login role that owns them (the one that runs the migrations) is not subject to the policies. With `DB_RLS=true` the API switches a connection to `nexora_app` (`set_config('role', ...)`) and sets `app.society_id` from the signed-in user's token whenever the request has one; code with no signed-in user (sign-in, sweepers, webhooks, the console) stays on the login role. A connection never keeps a previous caller's setting (it is rewritten on every acquire when it differs). With the role set and no society, **no row is visible** (fails closed).
- **The API's login role must own the tables or have BYPASSRLS** (normally the role that runs the migrations). Connecting as some other non-owner role would apply the policies to platform code too.
- `database.Unscoped(ctx)` is the explicit opt-out for the rare query that must look across societies for a signed-in user. There are two, both asking "is this person in another society?": `resident.Service.Get` and `household.Service.isShared`.
- Not covered on purpose (global or per user): `users`, `devices`, `refresh_tokens`, `notification_prefs`, `notification_outbox`, `emergency_contacts`, `roles`, `permissions`, `role_permissions`, `features`, `plans`, `app_versions`, `audit_settings`, `platform_*`. A test fails when a new table is neither protected nor on this list.

## Turning it on (staging first)
1. Run the migrations (the role is created by migration 51). The migrating database user must be allowed to `SET ROLE nexora_app` (the migration grants it).
2. Start one instance with `DB_RLS=true`. Click through each app, run `make test-rls` in CI, watch the logs for empty lists.
3. Watch for a screen that suddenly shows nothing: that is a query that needs `Unscoped` or runs without a signed-in user's context. `TEST_RLS=1 go test ./internal/app` runs the entire suite as the restricted role; it found both existing cases.
4. Production: switch with `DB_RLS=true`; rollback is `DB_RLS=false` and a restart (the policies stay, harmless without the role).

## Debugging an "empty list"
- Reproduce with `TEST_RLS=1`. In `psql` as the app role: `SET ROLE nexora_app; SELECT set_config('app.society_id','<id>',false); SELECT ...;  RESET ROLE;`
- A query using `context.Background()` or a detached context gets `bypass` (sees everything); a query whose **signed-in** context touches another society's rows sees nothing: use the society's id in the query, or `Unscoped` if cross-society is the point.
- Policies call `app_society()`; `\d+ tablename` shows them.

## Migrations and RLS
Create a table with `society_id`, then in the same migration: `ALTER TABLE t ENABLE ROW LEVEL SECURITY; CREATE POLICY tenant_isolation ON t USING (society_id = (SELECT app_society())) WITH CHECK (society_id = (SELECT app_society()));` (copy from migration 51). The migrating role needs no special handling: default privileges already grant `nexora_app` access to new tables.
