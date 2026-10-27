#!/usr/bin/env bash
# Logical backup of the Nexora database (pg_dump, custom format, compressed).
#
#   DATABASE_URL=postgres://... BACKUP_DIR=/var/backups/nexora scripts/backup.sh
#
# Optional:
#   BACKUP_S3_URI=s3://bucket/nexora     copy the dump there with `aws s3 cp` (set AWS_ENDPOINT_URL for MinIO)
#   BACKUP_HEARTBEAT_FILE=/var/backups/nexora/backup.last   touched after a good backup; the SuperAdmin System page reads its age
#   KEEP_DAILY=14 KEEP_WEEKLY=8 KEEP_MONTHLY=12             retention (weekly = Sundays, monthly = the 1st)
#   PG_BIN=/opt/homebrew/opt/libpq/bin                      where pg_dump lives if it is not on PATH
#
# The dump is written to a temporary name and renamed only when pg_dump succeeded and pg_restore can list it,
# so a half-written file is never mistaken for a backup.
set -euo pipefail

: "${DATABASE_URL:?set DATABASE_URL}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
KEEP_DAILY="${KEEP_DAILY:-14}"; KEEP_WEEKLY="${KEEP_WEEKLY:-8}"; KEEP_MONTHLY="${KEEP_MONTHLY:-12}"
[ -n "${PG_BIN:-}" ] && PATH="$PG_BIN:$PATH"
command -v pg_dump >/dev/null || { echo "backup: pg_dump not found (install postgresql-client or set PG_BIN)" >&2; exit 2; }

mkdir -p "$BACKUP_DIR"
stamp="$(date -u +%Y%m%d-%H%M%S)"
tmp="$BACKUP_DIR/.nexora-$stamp.dump.partial"
final="$BACKUP_DIR/nexora-$stamp.dump"

trap 'rm -f "$tmp"' EXIT
pg_dump --format=custom --compress=6 --no-owner --no-privileges --file="$tmp" "$DATABASE_URL"
pg_restore --list "$tmp" >/dev/null      # a dump that cannot be listed is not a backup
mv "$tmp" "$final"
trap - EXIT
( cd "$BACKUP_DIR" && shasum -a 256 "nexora-$stamp.dump" > "nexora-$stamp.dump.sha256" )
echo "backup: wrote $final ($(du -h "$final" | cut -f1))"

if [ -n "${BACKUP_S3_URI:-}" ]; then
  command -v aws >/dev/null || { echo "backup: BACKUP_S3_URI is set but the aws CLI is missing" >&2; exit 3; }
  aws s3 cp "$final" "$BACKUP_S3_URI/" ${AWS_ENDPOINT_URL:+--endpoint-url "$AWS_ENDPOINT_URL"}
  aws s3 cp "$final.sha256" "$BACKUP_S3_URI/" ${AWS_ENDPOINT_URL:+--endpoint-url "$AWS_ENDPOINT_URL"}
fi

# retention: keep the newest KEEP_DAILY dumps, plus the newest dump of each of the last KEEP_WEEKLY Sundays and
# KEEP_MONTHLY first-of-months. Everything else is removed.
python3 - "$BACKUP_DIR" "$KEEP_DAILY" "$KEEP_WEEKLY" "$KEEP_MONTHLY" <<'PY'
import os, sys, datetime
d, kd, kw, km = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
files = sorted(f for f in os.listdir(d) if f.startswith("nexora-") and f.endswith(".dump"))
def when(f): return datetime.datetime.strptime(f[7:22], "%Y%m%d-%H%M%S")
keep = set(files[-kd:])
weekly = [f for f in files if when(f).weekday() == 6]
monthly = [f for f in files if when(f).day == 1]
keep |= set(weekly[-kw:]) | set(monthly[-km:])
for f in files:
    if f not in keep:
        for ext in ("", ".sha256"):
            try: os.remove(os.path.join(d, f + ext))
            except FileNotFoundError: pass
        print("backup: removed old", f)
PY

if [ -n "${BACKUP_HEARTBEAT_FILE:-}" ]; then date -u +%s > "$BACKUP_HEARTBEAT_FILE"; fi
