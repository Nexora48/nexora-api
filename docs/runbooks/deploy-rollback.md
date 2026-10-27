# Deploy and rollback

You deploy by hand. These are the steps and the rules that make them safe.

## Rules for migrations (expand, then contract)
- Never edit a migration that has shipped. Add a new one.
- A release must work with the **previous** schema and the **next** one: first release adds columns/tables (nullable or defaulted), a later release starts using them, a still later one drops what is unused.
- Every migration has a `.down.sql`, but treat rollback of data-changing migrations as a restore from backup, not a `down`.
- New tenant tables **must** have a `society_id` and row-level security. `TestEveryTableIsProtectedOrExemptOnPurpose` fails the build otherwise.
- Index on a big table: `CREATE INDEX CONCURRENTLY` in its own migration file.

## Release
1. `make test`, `make test-rls`, `make test-pool1` green on the commit you will ship; `go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...` clean.
2. **Take a backup now**: `scripts/backup.sh` (and check `docs/runbooks/backup-restore.md` if the last restore drill is older than a month).
3. Build the image: `docker build -f deployments/Dockerfile --build-arg APP_COMMIT=$(git rev-parse HEAD) -t nexora-api:<tag> .` and keep the previous tag.
4. Start the new version with `AUTO_MIGRATE=true` on **one** instance first (migrations take a lock; two instances starting together are safe but noisy). Watch `/ready`, `/metrics` and the SuperAdmin System page (version, commit, migration number, checks).
5. Roll the other instances. Watch the API dashboard (5xx ratio, p95, pool busy) for 15 minutes.

## Rollback
- **Code only** (no migration in the release): start the previous image. Done.
- **With a migration**: if the new schema is backward compatible (expand step), start the previous image; the extra columns do no harm. If it is not, restore the backup taken in step 2 into a scratch database, compare, then switch (`docs/runbooks/backup-restore.md`). Do not run `down` migrations against production data without a restore plan.
- Apps: the minimum/latest app version switch (SuperAdmin → System) can stop an old or broken build from running; it fails open if the API is unreachable.

## Configuration changes
`APP_ENV` other than `dev`/`test` runs `config.Validate` at start. The process exits with a list of what is unsafe: fix the environment, do not bypass the check.
