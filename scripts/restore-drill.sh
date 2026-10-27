#!/usr/bin/env bash
# The restore drill: take the newest backup, restore it into a scratch database, and prove it is usable by
# comparing the migration version and the row counts of every table against the live database.
#
#   DATABASE_URL=postgres://.../nexora ADMIN_URL=postgres://.../postgres BACKUP_DIR=./backups scripts/restore-drill.sh
#
# ADMIN_URL must be allowed to CREATE and DROP databases. The live database is only read.
set -euo pipefail
: "${DATABASE_URL:?}"; : "${ADMIN_URL:?}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
[ -n "${PG_BIN:-}" ] && PATH="$PG_BIN:$PATH"
here="$(cd "$(dirname "$0")" && pwd)"
dump="$(ls -1t "$BACKUP_DIR"/nexora-*.dump 2>/dev/null | head -1 || true)"
[ -n "$dump" ] || { echo "drill: no backup in $BACKUP_DIR" >&2; exit 2; }

name="restore_drill_$(date -u +%Y%m%d%H%M%S)"
psql "$ADMIN_URL" -qc "CREATE DATABASE $name"
cleanup() { psql "$ADMIN_URL" -qc "DROP DATABASE IF EXISTS $name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

scratch="$(python3 - "$ADMIN_URL" "$name" <<'PY'
import sys, re
u, n = sys.argv[1], sys.argv[2]
print(re.sub(r'/[^/?]*(\?|$)', '/' + n + r'\1', u, count=1))
PY
)"
TARGET_URL="$scratch" "$here/restore.sh" "$dump"

count_sql="SELECT table_name || '=' || (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')))[1]::text
           FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE' ORDER BY 1"
live="$(psql "$DATABASE_URL" -Atc "$count_sql")"
rest="$(psql "$scratch" -Atc "$count_sql")"
ver_live="$(psql "$DATABASE_URL" -Atc "SELECT version FROM schema_migrations")"
ver_rest="$(psql "$scratch" -Atc "SELECT version FROM schema_migrations")"
if [ "$ver_live" != "$ver_rest" ]; then echo "drill: FAILED, migration version $ver_rest restored vs $ver_live live" >&2; exit 5; fi
# tables written to since the backup was taken may differ; only a table that is EMPTY after restore but not live is an error
bad=0
while IFS='=' read -r t n; do
  r="$(printf '%s\n' "$rest" | awk -F= -v t="$t" '$1==t{print $2}')"
  if [ "${n:-0}" != "0" ] && [ "${r:-0}" = "0" ]; then echo "drill: table $t has $n rows live but 0 after restore" >&2; bad=1; fi
done <<<"$live"
[ "$bad" = "0" ] || exit 6
echo "drill: OK ($dump restored, migration $ver_rest, $(printf '%s\n' "$rest" | wc -l | tr -d ' ') tables)"
[ -n "${BACKUP_HEARTBEAT_FILE:-}" ] && date -u +%s > "$BACKUP_HEARTBEAT_FILE.drill" || true
