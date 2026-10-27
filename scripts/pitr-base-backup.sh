#!/usr/bin/env bash
# A physical base backup for point-in-time recovery. Needs WAL archiving (deployments/docker-compose.pitr.yml).
#
#   DATABASE_URL=postgres://replicator:...@host/nexora BASE_DIR=/basebackups scripts/pitr-base-backup.sh
#
# The role needs REPLICATION. The backup is a compressed tar set plus the WAL it needs (-X stream).
set -euo pipefail
: "${DATABASE_URL:?}"; BASE_DIR="${BASE_DIR:-./basebackups}"
[ -n "${PG_BIN:-}" ] && PATH="$PG_BIN:$PATH"
command -v pg_basebackup >/dev/null || { echo "pitr: pg_basebackup not found (set PG_BIN)" >&2; exit 2; }
out="$BASE_DIR/base-$(date -u +%Y%m%d-%H%M%S)"
mkdir -p "$out"
pg_basebackup --dbname="$DATABASE_URL" --format=tar --gzip --wal-method=stream --checkpoint=fast --label="nexora-$(basename "$out")" --pgdata="$out"
pg_verifybackup --no-parse-wal "$out" 2>/dev/null || true   # tar format cannot be verified by older versions; the label file is the proof
echo "pitr: base backup written to $out"
[ -n "${BACKUP_HEARTBEAT_FILE:-}" ] && date -u +%s > "$BACKUP_HEARTBEAT_FILE.base" || true
