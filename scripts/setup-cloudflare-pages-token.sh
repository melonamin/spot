#!/bin/sh
set -eu

api_base=${CLOUDFLARE_API_BASE_URL:-https://api.cloudflare.com/client/v4}
prefix=spot-
name=spot-cloudflare-pages-runtime
bootstrap_token=
account_id=
zone_id=
base_domain=

usage() {
    cat <<'USAGE'
Usage: scripts/setup-cloudflare-pages-token.sh \
  --bootstrap-token TOKEN \
  --account-id ACCOUNT_ID \
  --zone-id ZONE_ID \
  --base-domain pages.example.com [--project-prefix spot-]

Creates a lower-permission Cloudflare runtime API token and prints the
SPOT_CLOUDFLARE_* environment variables for Spot. Requires curl and jq.
The bootstrap token is used only for these API calls and is never stored.
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --bootstrap-token)
            bootstrap_token=${2:-}
            shift 2
            ;;
        --account-id)
            account_id=${2:-}
            shift 2
            ;;
        --zone-id)
            zone_id=${2:-}
            shift 2
            ;;
        --base-domain)
            base_domain=${2:-}
            shift 2
            ;;
        --project-prefix)
            prefix=${2:-}
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "unknown argument: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

if [ -z "$bootstrap_token" ] || [ -z "$account_id" ] || [ -z "$zone_id" ] || [ -z "$base_domain" ]; then
    usage >&2
    exit 2
fi

if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required" >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required" >&2
    exit 1
fi

cf() {
    method=$1
    path=$2
    body=${3:-}
    if [ -n "$body" ]; then
        curl -fsS -X "$method" "$api_base$path" \
            -H "Authorization: Bearer $bootstrap_token" \
            -H "Content-Type: application/json" \
            --data "$body"
    else
        curl -fsS -X "$method" "$api_base$path" \
            -H "Authorization: Bearer $bootstrap_token"
    fi
}

groups=$(cf GET /user/tokens/permission_groups)

permission_id() {
    printf '%s' "$groups" | jq -er --arg name "$1" '
      .result[]
      | select((.name | ascii_downcase) == ($name | ascii_downcase))
      | .id
    ' 2>/dev/null
}

first_permission_id() {
    for name in "$@"; do
        id=$(permission_id "$name" || true)
        if [ -n "$id" ]; then
            printf '%s\n' "$id"
            return 0
        fi
    done
    return 1
}

pages_write=$(first_permission_id "Cloudflare Pages Write" "Pages Write" "Workers Pages Write" "Workers Scripts Write") || {
    echo "could not find a Cloudflare Pages write permission group" >&2
    exit 1
}
dns_write=$(first_permission_id "DNS Write" "Zone DNS Write") || {
    echo "could not find a DNS Write permission group" >&2
    exit 1
}
zone_read=$(first_permission_id "Zone Read") || {
    echo "could not find a Zone Read permission group" >&2
    exit 1
}

payload=$(jq -n \
    --arg name "$name" \
    --arg account "$account_id" \
    --arg zone "$zone_id" \
    --arg pages "$pages_write" \
    --arg dns "$dns_write" \
    --arg zone_read "$zone_read" '
{
  name: $name,
  policies: [
    {
      effect: "allow",
      resources: {("com.cloudflare.api.account." + $account): "*"},
      permission_groups: [{id: $pages}]
    },
    {
      effect: "allow",
      resources: {("com.cloudflare.api.account.zone." + $zone): "*"},
      permission_groups: [{id: $dns}, {id: $zone_read}]
    }
  ]
}')

created=$(cf POST /user/tokens "$payload")
runtime_token=$(printf '%s' "$created" | jq -er '.result.value')

cat <<EOF
SPOT_CLOUDFLARE_API_TOKEN=$runtime_token
SPOT_CLOUDFLARE_ACCOUNT_ID=$account_id
SPOT_CLOUDFLARE_ZONE_ID=$zone_id
SPOT_CLOUDFLARE_BASE_DOMAIN=$base_domain
SPOT_CLOUDFLARE_PROJECT_PREFIX=$prefix
EOF
