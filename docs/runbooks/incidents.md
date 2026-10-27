# Society reports wrong or missing data; suspending a tenant

## "I can't see X" / "this shows the wrong flat"
1. Ask for the society, the screen and the time. Find the request: log search by society id is not possible by design (logs carry request ids, Sentry tags carry society ids): ask the user to retry and read the request id from the failing response.
2. Check scope first: is the user's **membership** active (status, `app_access`), do they have the permission, is the **service switch** on for the society, did a **plan limit** refuse the action (`PLAN_LIMIT`)? SuperAdmin → Societies → the society → Services / Usage show the last two.
3. If lists are empty only with `DB_RLS=true`: `docs/runbooks/rls.md` → debugging an empty list.
4. Data was changed? SuperAdmin → Audit log → filter by the society and the entity; open an entry for before/after.
5. Wrong data after a bad import or edit: fix with the app's own action (void an invoice, move a resident out) so the audit trail and ledger stay consistent. Direct SQL is a last resort: take a backup first and write down what you ran.

## Suspend a tenant during an incident
SuperAdmin → Societies → the society → **Suspend** with a reason. Sign-in and every call for that society return `SOCIETY_SUSPENDED`; nothing is deleted. Reactivate from the same page. Suspension is audited.

## Stop a compromised account
- Society user: Admin → Residents/Guards → disable app access, or SuperAdmin → society → Admins → reset password.
- Staff: Staff → … → Disable (their tokens stop working immediately) and Reset authenticator.

## After the incident
Write down: what happened, when it started and ended, who was affected, what fixed it, what would have caught it earlier (an alert? a test?). Add the test.
