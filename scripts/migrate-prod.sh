#!/bin/sh
# Stream the current migration tool to the production host and run it
# against the existing production checkout, before deploy-prod replaces
# the old compose files.
set -eu

cd "$(dirname "$0")/.."

host=${SPOT_DEPLOY_HOST:-ubuntu@spot.t1a.dev}
dir=${SPOT_DEPLOY_DIR:-/home/ubuntu/spot}

usage() {
    cat <<'EOF'
usage: scripts/migrate-prod.sh

Environment:
  SPOT_DEPLOY_HOST             SSH target (default: ubuntu@spot.t1a.dev)
  SPOT_DEPLOY_DIR              remote app dir (default: /home/ubuntu/spot)
  SPOT_MIGRATE_OVERWRITE=1     replace existing SQLite migration output
  SPOT_MIGRATE_SKIP_VOLUME=1   do not copy spot.db into spot_spot-data
  SPOT_MIGRATE_COMPOSE         remote compose command for the old stack
  SPOT_MIGRATE_BACKUP_DIR      remote backup/export directory
  SPOT_MIGRATE_SQLITE_PATH     remote SQLite file to create
  SPOT_MIGRATE_SQLITE_VOLUME   target Docker volume, default spot_spot-data
EOF
}

quote() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

case "${1:-}" in
    --help|-h)
        usage
        exit 0
        ;;
    "")
        ;;
    *)
        echo "error: unknown option '$1'" >&2
        usage >&2
        exit 1
        ;;
esac

remote_script='
set -eu
tmp=$(mktemp /tmp/spot-migrate.XXXXXX.sh)
trap '\''rm -f "$tmp"'\'' EXIT
cat > "$tmp"
chmod +x "$tmp"
SPOT_MIGRATE_APP_DIR="$SPOT_DEPLOY_DIR" \
SPOT_MIGRATE_OVERWRITE="${SPOT_MIGRATE_OVERWRITE:-}" \
SPOT_MIGRATE_SKIP_VOLUME="${SPOT_MIGRATE_SKIP_VOLUME:-}" \
SPOT_MIGRATE_COMPOSE="${SPOT_MIGRATE_COMPOSE:-}" \
SPOT_MIGRATE_BACKUP_DIR="${SPOT_MIGRATE_BACKUP_DIR:-}" \
SPOT_MIGRATE_SQLITE_PATH="${SPOT_MIGRATE_SQLITE_PATH:-}" \
SPOT_MIGRATE_SQLITE_VOLUME="${SPOT_MIGRATE_SQLITE_VOLUME:-}" \
    "$tmp"
'

ssh "$host" \
    "SPOT_DEPLOY_DIR=$(quote "$dir") SPOT_MIGRATE_OVERWRITE=$(quote "${SPOT_MIGRATE_OVERWRITE:-}") SPOT_MIGRATE_SKIP_VOLUME=$(quote "${SPOT_MIGRATE_SKIP_VOLUME:-}") SPOT_MIGRATE_COMPOSE=$(quote "${SPOT_MIGRATE_COMPOSE:-}") SPOT_MIGRATE_BACKUP_DIR=$(quote "${SPOT_MIGRATE_BACKUP_DIR:-}") SPOT_MIGRATE_SQLITE_PATH=$(quote "${SPOT_MIGRATE_SQLITE_PATH:-}") SPOT_MIGRATE_SQLITE_VOLUME=$(quote "${SPOT_MIGRATE_SQLITE_VOLUME:-}") sh -c $(quote "$remote_script")" \
    < scripts/migrate-postgres-to-sqlite.sh
