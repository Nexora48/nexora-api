# Backups, restore and point-in-time recovery

## What is backed up
| Data | How | Frequency |
|---|---|---|
| PostgreSQL | `scripts/backup.sh` (pg_dump custom format, checksum, atomic rename) | daily |
| PostgreSQL, to the minute | WAL archiving + base backup (`deployments/docker-compose.pitr.yml`, `scripts/pitr-base-backup.sh`) | continuous + weekly base |
| Photos/attachments | copy of `BLOB_PATH` (rsync/`aws s3 sync`) after the database backup | daily |
| Redis | **not backed up on purpose** (see `redis.md`) | – |

Retention for dumps: 14 daily, 8 weekly (Sundays), 12 monthly (the 1st); `KEEP_DAILY/WEEKLY/MONTHLY` change it. Dumps are also copied off the machine with `BACKUP_S3_URI`.

Use `pg_dump` and `psql` of the **same major version as the server** (16): a newer client writes settings an older server rejects.

## Take a backup
```bash
DATABASE_URL=postgres://... BACKUP_DIR=/var/backups/nexora BACKUP_HEARTBEAT_FILE=/var/backups/nexora/backup.last scripts/backup.sh
```
The System page shows the age of `backup.last` (warn after 26 h, fail after 50 h) and `nexora_backup_age_seconds` feeds the `BackupStale` alert.

## The restore drill (do this monthly)
```bash
DATABASE_URL=postgres://.../nexora ADMIN_URL=postgres://.../postgres BACKUP_DIR=/var/backups/nexora scripts/restore-drill.sh
```
It restores the newest dump into a scratch database, compares the migration version and that no table that has rows live is empty after restore, and drops the scratch database. `drill: OK` means the backup is usable. The same drill runs in the test suite (`TestBackupAndRestoreDrill`).

## Restore for real
1. Stop the API (or put it in maintenance) so nothing writes.
2. Restore into a **new** database, check it, then switch `DATABASE_URL`:
   ```bash
   TARGET_URL=postgres://.../nexora_restored scripts/restore.sh /var/backups/nexora/nexora-YYYYmmdd-HHMMSS.dump
   ```
   The script verifies the checksum first and refuses a non-empty target unless `FORCE=1`.
3. Restore the blob copy from the same day; run `go run ./cmd/blobcheck` (exit 0 = nothing missing).
4. Start the API; check `/ready`, the System page and one society's data. Tell the societies how far back the data goes.

## Point-in-time recovery (to a minute)
Needs WAL archiving on **before** the incident (`archive_mode=on`, `archive_command` copying every segment; `archive_timeout=60` bounds the loss to about a minute).
1. Stop PostgreSQL. Move the damaged data directory aside.
2. Unpack the newest base backup (`scripts/pitr-base-backup.sh` output) into an empty data directory.
3. Create `recovery.signal` and set in `postgresql.conf`:
   ```
   restore_command = 'cp /walarchive/%f %p'
   recovery_target_time = '2026-11-02 14:35:00+05:30'   # the minute BEFORE the mistake
   recovery_target_action = 'promote'
   ```
4. Start PostgreSQL; it replays WAL to that time and promotes. Check the data, then point the API at it.
In production swap `cp` for `pgbackrest`/`wal-g` and S3 storage; the steps stay the same. **This path was written but not run in M11** (no Docker on the build machine): rehearse it once on staging before you rely on it.

## Total loss of the machine
New machine → install Postgres 16 → restore the latest dump (or PITR) → copy blobs → deploy the API image → set the environment from your secret manager. Secrets are **not** in the database dump: keep `PLATFORM_ENC_KEY` safe, or stored gateway keys and TOTP secrets cannot be decrypted.
