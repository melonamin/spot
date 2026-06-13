#!/bin/sh
# Deploy the committed Spot tree to the production VM.
set -eu

host=${SPOT_DEPLOY_HOST:-ubuntu@spot.t1a.dev}
dir=${SPOT_DEPLOY_DIR:-/home/ubuntu/spot}
compose=${SPOT_DEPLOY_COMPOSE:-docker compose -f docker-compose.yml -f docker-compose.mesh.yml}
dry_run=0

usage() {
    cat <<'EOF'
usage: scripts/deploy-prod.sh [--dry-run]

Environment:
  SPOT_DEPLOY_HOST      SSH target (default: ubuntu@spot.t1a.dev)
  SPOT_DEPLOY_DIR       remote app dir (default: /home/ubuntu/spot)
  SPOT_DEPLOY_COMPOSE   remote compose command
  SPOT_DEPLOY_ALLOW_DIRTY=1  allow deploying when tracked files are dirty
EOF
}

quote() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

while [ $# -gt 0 ]; do
    case $1 in
        --dry-run)
            dry_run=1
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "error: unknown option '$1'" >&2
            usage >&2
            exit 1
            ;;
    esac
done

if ! git diff --quiet || ! git diff --cached --quiet; then
    if [ "${SPOT_DEPLOY_ALLOW_DIRTY:-}" != 1 ]; then
        echo "error: tracked files have uncommitted changes; commit them before deploying" >&2
        echo "       or set SPOT_DEPLOY_ALLOW_DIRTY=1 to deploy the current HEAD anyway" >&2
        exit 1
    fi
fi

commit=$(git rev-parse --short HEAD)

echo "deploying Spot $commit"
echo "  host: $host"
echo "  dir:  $dir"

if [ "$dry_run" = 1 ]; then
    echo "  mode: dry-run"
    echo
    echo "tracked top-level entries that would be replaced:"
    git ls-tree --name-only HEAD | sed 's/^/  - /'
    exit 0
fi

remote_script='
set -eu
tmp=$(mktemp /tmp/spot-deploy.XXXXXX.tar)
trap '\''rm -f "$tmp"'\'' EXIT
cat > "$tmp"
mkdir -p "$SPOT_DEPLOY_DIR"
cd "$SPOT_DEPLOY_DIR"
tar -tf "$tmp" >/dev/null
tar -tf "$tmp" | sed '\''s#/.*##'\'' | sort -u | while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    rm -rf -- "$entry"
done
tar -xpf "$tmp"
eval "$SPOT_DEPLOY_COMPOSE up -d --build"
eval "$SPOT_DEPLOY_COMPOSE ps"
'

git archive --format=tar HEAD | ssh "$host" \
    "SPOT_DEPLOY_DIR=$(quote "$dir") SPOT_DEPLOY_COMPOSE=$(quote "$compose") sh -c $(quote "$remote_script")"
