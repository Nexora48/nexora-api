#!/usr/bin/env bash
# Restore a dump made by backup.sh into a database.
#
#   TARGET_URL=postgres://.../nexora_restored scripts/restore.sh backups/nexora-20261027-020000.dump
#
# Refuses to touch a database that already has tables unless FORCE=1 (then objects are dropped first).
# Never defaults to DATABASE_URL: restoring over production must be a deliberate, typed target.
set -euo pipefail
dump="${1:?usage: restore.sh <dump file>}"
: "${TARGET_URL:?set TARGET_URL to the database to restore into}"
[ -n "${PG_BIN:-}" ] && PATH="$PG_BIN:$PATH"
command -v pg_restore >/dev/null || { echo "restore: pg_restore not found (set PG_BIN)" >&2; exit 2; }
[ -f "$dump" ] || { echo "restore: $dump not found" >&2; exit 2; }
if [ -f "$dump.sha256" ]; then ( cd "$(dirname "$dump")" && shasum -a 256 -c "$(basename "$dump").sha256" >/dev/null ) || { echo "restore: checksum mismatch, the dump is damaged" >&2; exit 4; }; fi

tables="$(psql "$TARGET_URL" -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")"
if [ "$tables" != "0" ] && [ "${FORCE:-}" != "1" ]; then
  echo "restore: the target already has $tables tables; set FORCE=1 to replace them" >&2
  exit 3
fi
extra=""; [ "${FORCE:-}" = "1" ] && extra="--clean --if-exists"
# shellcheck disable=SC2086
pg_restore --no-owner --no-privileges --exit-on-error $extra --dbname="$TARGET_URL" "$dump"
echo "restore: $dump restored into the target ($(psql "$TARGET_URL" -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'") tables)"
