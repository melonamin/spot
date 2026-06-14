#!/usr/bin/env bash
# One-time migration from the old Postgres metadata database to the new
# SQLite database used by spot-api.
set -euo pipefail

if [ -n "${SPOT_MIGRATE_APP_DIR:-}" ]; then
    cd "$SPOT_MIGRATE_APP_DIR"
else
    case ${BASH_SOURCE[0]} in
        */*) cd "$(dirname "${BASH_SOURCE[0]}")/.." ;;
    esac
fi

compose_raw=${SPOT_MIGRATE_COMPOSE:-"docker compose -f docker-compose.yml -f docker-compose.mesh.yml"}
read -r -a compose <<<"$compose_raw"

backup_dir=${SPOT_MIGRATE_BACKUP_DIR:-"data/migrations/$(date -u +%Y%m%dT%H%M%SZ)-postgres-to-sqlite"}
sqlite_path=${SPOT_MIGRATE_SQLITE_PATH:-"$backup_dir/spot.db"}
sqlite_volume=${SPOT_MIGRATE_SQLITE_VOLUME:-spot_spot-data}
postgres_service=${SPOT_MIGRATE_POSTGRES_SERVICE:-postgres}

usage() {
    cat <<'EOF'
usage: scripts/migrate-postgres-to-sqlite.sh

Exports the old compose Postgres metadata tables, writes a SQLite
database, validates row counts, and copies the database into the compose
SQLite data volume for the new spot-api.

Environment:
  SPOT_MIGRATE_COMPOSE           compose command for the old stack
                                 default: docker compose -f docker-compose.yml -f docker-compose.mesh.yml
  SPOT_MIGRATE_BACKUP_DIR        backup/export directory
  SPOT_MIGRATE_SQLITE_PATH       SQLite file to create
  SPOT_MIGRATE_SQLITE_VOLUME     target Docker volume, default spot_spot-data
  SPOT_MIGRATE_POSTGRES_SERVICE  old Postgres service name, default postgres
  SPOT_MIGRATE_OVERWRITE=1       allow replacing an existing SQLite output/volume DB
  SPOT_MIGRATE_SKIP_VOLUME=1     create/validate the SQLite file but do not copy to a Docker volume
EOF
}

for arg in "$@"; do
    case "$arg" in
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "error: unknown argument $arg" >&2
            usage >&2
            exit 1
            ;;
    esac
done

need() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: missing required command: $1" >&2
        exit 1
    fi
}

need docker
need python3

if [ -e "$sqlite_path" ] && [ "${SPOT_MIGRATE_OVERWRITE:-}" != 1 ]; then
    echo "error: $sqlite_path already exists; set SPOT_MIGRATE_OVERWRITE=1 to replace it" >&2
    exit 1
fi

mkdir -p "$backup_dir"
mkdir -p "$(dirname "$sqlite_path")"

psql_exec() {
    "${compose[@]}" exec -T "$postgres_service" sh -lc \
        'export PGPASSWORD="${POSTGRES_PASSWORD:-spot}"; exec psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-spot}" -d "${POSTGRES_DB:-spot}"' "$@"
}

pg_dump_exec() {
    "${compose[@]}" exec -T "$postgres_service" sh -lc \
        'export PGPASSWORD="${POSTGRES_PASSWORD:-spot}"; exec pg_dump -Fc -U "${POSTGRES_USER:-spot}" -d "${POSTGRES_DB:-spot}"'
}

service_exists() {
    "${compose[@]}" config --services | grep -qx "$1"
}

stop_if_present() {
    if service_exists "$1"; then
        "${compose[@]}" stop "$1"
    fi
}

echo "==> stopping old request-serving services"
stop_if_present spot-api
stop_if_present caddy

echo "==> starting old Postgres service"
"${compose[@]}" up -d "$postgres_service"

echo "==> waiting for Postgres"
for _ in $(seq 1 60); do
    if "${compose[@]}" exec -T "$postgres_service" sh -lc \
        'export PGPASSWORD="${POSTGRES_PASSWORD:-spot}"; pg_isready -U "${POSTGRES_USER:-spot}" -d "${POSTGRES_DB:-spot}"' >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
if [ "${ready:-}" != 1 ]; then
    echo "error: Postgres did not become ready" >&2
    exit 1
fi

echo "==> writing Postgres backup"
pg_dump_exec >"$backup_dir/postgres.dump"

cat >"$backup_dir/counts.sql" <<'SQL'
SELECT 'documents', count(*) FROM documents
UNION ALL SELECT 'sites', count(*) FROM sites
UNION ALL SELECT 'site_deploy_audit', count(*) FROM site_deploy_audit
ORDER BY 1;
SQL
psql_exec <"$backup_dir/counts.sql" >"$backup_dir/postgres-counts.txt"

echo "==> exporting tables as CSV"
cat >"$backup_dir/export-documents.sql" <<'SQL'
COPY (
    SELECT id::text,
           scope,
           collection,
           data::text,
           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') AS created_at,
           to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') AS updated_at
    FROM documents
    ORDER BY created_at, id
) TO STDOUT WITH CSV HEADER;
SQL
psql_exec <"$backup_dir/export-documents.sql" >"$backup_dir/documents.csv"

cat >"$backup_dir/export-sites.sql" <<'SQL'
COPY (
    SELECT name,
           owner_email,
           owner_peer_ip,
           owner_name,
           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') AS created_at,
           to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') AS updated_at
    FROM sites
    ORDER BY created_at, name
) TO STDOUT WITH CSV HEADER;
SQL
psql_exec <"$backup_dir/export-sites.sql" >"$backup_dir/sites.csv"

cat >"$backup_dir/export-site-deploy-audit.sql" <<'SQL'
COPY (
    SELECT id,
           site,
           actor_email,
           actor_peer_ip,
           actor_name,
           actor_groups::text,
           action,
           status,
           file_count,
           total_bytes,
           message,
           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') AS created_at
    FROM site_deploy_audit
    ORDER BY id
) TO STDOUT WITH CSV HEADER;
SQL
psql_exec <"$backup_dir/export-site-deploy-audit.sql" >"$backup_dir/site_deploy_audit.csv"

echo "==> creating SQLite database"
rm -f "$sqlite_path" "$sqlite_path-shm" "$sqlite_path-wal"
python3 - "$backup_dir" "$sqlite_path" <<'PY'
import csv
import json
import sqlite3
import sys
from pathlib import Path

backup = Path(sys.argv[1])
sqlite_path = Path(sys.argv[2])

schema = """
CREATE TABLE IF NOT EXISTS documents (
    id text PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6)))),
    scope text NOT NULL,
    collection text NOT NULL,
    data text NOT NULL,
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS documents_scope_collection_idx
    ON documents (scope, collection, created_at DESC);

CREATE TABLE IF NOT EXISTS sites (
    name text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE TABLE IF NOT EXISTS site_deploy_audit (
    id integer PRIMARY KEY AUTOINCREMENT,
    site text NOT NULL,
    actor_email text NOT NULL DEFAULT '',
    actor_peer_ip text NOT NULL DEFAULT '',
    actor_name text NOT NULL DEFAULT '',
    actor_groups text NOT NULL DEFAULT '[]',
    action text NOT NULL,
    status text NOT NULL,
    file_count integer NOT NULL DEFAULT 0,
    total_bytes integer NOT NULL DEFAULT 0,
    message text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS site_deploy_audit_site_created_idx
    ON site_deploy_audit (site, created_at DESC);
"""

def rows(name):
    with (backup / name).open(newline="") as f:
        yield from csv.DictReader(f)

def compact_json(raw, fallback):
    raw = raw if raw else fallback
    return json.dumps(json.loads(raw), separators=(",", ":"), sort_keys=False)

conn = sqlite3.connect(sqlite_path)
try:
    conn.executescript(schema)
    with conn:
        doc_count = 0
        for row in rows("documents.csv"):
            conn.execute(
                "INSERT INTO documents (id, scope, collection, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
                (
                    row["id"],
                    row["scope"],
                    row["collection"],
                    compact_json(row["data"], "{}"),
                    row["created_at"],
                    row["updated_at"],
                ),
            )
            doc_count += 1

        site_count = 0
        for row in rows("sites.csv"):
            conn.execute(
                "INSERT INTO sites (name, owner_email, owner_peer_ip, owner_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
                (
                    row["name"],
                    row["owner_email"],
                    row["owner_peer_ip"],
                    row["owner_name"],
                    row["created_at"],
                    row["updated_at"],
                ),
            )
            site_count += 1

        audit_count = 0
        max_audit_id = 0
        for row in rows("site_deploy_audit.csv"):
            audit_id = int(row["id"])
            max_audit_id = max(max_audit_id, audit_id)
            conn.execute(
                """INSERT INTO site_deploy_audit
                   (id, site, actor_email, actor_peer_ip, actor_name, actor_groups,
                    action, status, file_count, total_bytes, message, created_at)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    audit_id,
                    row["site"],
                    row["actor_email"],
                    row["actor_peer_ip"],
                    row["actor_name"],
                    compact_json(row["actor_groups"], "[]"),
                    row["action"],
                    row["status"],
                    int(row["file_count"]),
                    int(row["total_bytes"]),
                    row["message"],
                    row["created_at"],
                ),
            )
            audit_count += 1
        if max_audit_id:
            conn.execute("DELETE FROM sqlite_sequence WHERE name = 'site_deploy_audit'")
            conn.execute(
                "INSERT INTO sqlite_sequence (name, seq) VALUES ('site_deploy_audit', ?)",
                (max_audit_id,),
            )

    actual = {
        "documents": conn.execute("SELECT count(*) FROM documents").fetchone()[0],
        "sites": conn.execute("SELECT count(*) FROM sites").fetchone()[0],
        "site_deploy_audit": conn.execute("SELECT count(*) FROM site_deploy_audit").fetchone()[0],
    }
    expected = {
        "documents": doc_count,
        "sites": site_count,
        "site_deploy_audit": audit_count,
    }
    if actual != expected:
        raise SystemExit(f"row-count mismatch: expected {expected}, got {actual}")
    integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
    if integrity != "ok":
        raise SystemExit(f"sqlite integrity_check failed: {integrity}")
    for key in ("documents", "sites", "site_deploy_audit"):
        print(f"{key}|{actual[key]}")
finally:
    conn.close()
PY

if [ "${SPOT_MIGRATE_SKIP_VOLUME:-}" = 1 ]; then
    echo "==> SQLite migration written to $sqlite_path"
    echo "==> skipped Docker volume install because SPOT_MIGRATE_SKIP_VOLUME=1"
    exit 0
fi

echo "==> installing SQLite database into Docker volume $sqlite_volume"
docker volume create "$sqlite_volume" >/dev/null
if docker run --rm -v "$sqlite_volume":/data:ro alpine:3.22 sh -c 'test ! -s /data/spot.db'; then
    :
elif [ "${SPOT_MIGRATE_OVERWRITE:-}" = 1 ]; then
    :
else
    echo "error: /data/spot.db already exists in $sqlite_volume; set SPOT_MIGRATE_OVERWRITE=1 to replace it" >&2
    exit 1
fi

sqlite_dir=$(cd "$(dirname "$sqlite_path")" && pwd)
sqlite_base=$(basename "$sqlite_path")
docker run --rm \
    -v "$sqlite_dir":/src:ro \
    -v "$sqlite_volume":/data \
    alpine:3.22 sh -c "cp /src/$sqlite_base /data/spot.db && rm -f /data/spot.db-shm /data/spot.db-wal && chown -R 65534:65534 /data"

echo "==> migration complete"
echo "backup:  $backup_dir"
echo "sqlite:  $sqlite_path"
echo "volume:  $sqlite_volume:/data/spot.db"
