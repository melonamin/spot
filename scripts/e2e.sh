#!/bin/sh
# End-to-end test: brings the full stack up, deploys the demo site through
# the real CLI, and exercises serving + the database API through Caddy.
set -eu

cd "$(dirname "$0")/.."

CURL="curl -sk --resolve demo.spot.localhost:8443:127.0.0.1 --resolve spot.localhost:8443:127.0.0.1"

fail() {
    echo "E2E FAIL: $1" >&2
    exit 1
}

echo "==> the served CLI matches the repo CLI"
# sdk/spot is the copy install.sh downloads from the apex; Caddy's mount
# can't follow a symlink out of sdk/, so a copy is structural — this
# guard is what keeps it from drifting.
diff -q cli/spot sdk/spot >/dev/null || fail "sdk/spot has drifted from cli/spot — copy cli/spot over it"

echo "==> starting stack"
mkdir -p data/sites
export SPOT_DEV_IDENTITY_EMAIL=${SPOT_DEV_IDENTITY_EMAIL:-e2e@localhost}
export SPOT_DEV_IDENTITY_NAME=${SPOT_DEV_IDENTITY_NAME:-Spot E2E}
export NETBIRD_API_URL=
export NETBIRD_API_TOKEN=
# The CLI must target the local stack — a developer's ~/.config/spot/env
# may pin SPOT_URL to a real deployment, and the sourced config file
# overrides even an exported SPOT_URL.
XDG_CONFIG_HOME="$(mktemp -d)"
export XDG_CONFIG_HOME
docker compose up -d --build
# The Caddyfile is a single-file bind mount, which pins the file's
# inode: a Caddyfile edited (replaced) on the host stays invisible to a
# running container, even with `caddy reload`. Recreate the container
# so it mounts the current file.
docker compose up -d --force-recreate caddy

echo "==> waiting for Caddy and the API"
ready=""
for _ in $(seq 1 60); do
    code=$($CURL -o /dev/null -w '%{http_code}' https://spot.localhost:8443/ 2>/dev/null || true)
    api=$($CURL -o /dev/null -w '%{http_code}' https://demo.spot.localhost:8443/api/me 2>/dev/null || true)
    if [ "$code" = "200" ] && [ "$api" != "000" ] && [ -n "$api" ]; then
        ready=1
        break
    fi
    sleep 1
done
[ -n "$ready" ] || fail "stack did not become ready within 60s"

echo "==> apex page serves the deploy UI"
$CURL https://spot.localhost:8443/ | grep -q "Drop your folder here" \
    || fail "apex page does not contain the deploy drop zone"

echo "==> deploying the demo site via the CLI"
( cd examples/demo && ../../cli/spot deploy demo )

echo "==> waiting for the rclone mount to see the deploy"
ok=""
for _ in $(seq 1 30); do
    if $CURL https://demo.spot.localhost:8443/ 2>/dev/null | grep -q "Spot demo guestbook"; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "deployed site not served at demo.spot.localhost"
echo "    site is live"

echo "==> database API: create"
created=$($CURL -X POST -H 'Content-Type: application/json' \
    -d '{"message":"e2e was here"}' \
    https://demo.spot.localhost:8443/api/db/entries)
echo "$created" | grep -q '"id"' || fail "create did not return a document: $created"
id=$(echo "$created" | sed -n 's/.*"id":"\([0-9a-f-]*\)".*/\1/p')

echo "==> database API: list"
$CURL https://demo.spot.localhost:8443/api/db/entries | grep -q "e2e was here" \
    || fail "created document missing from list"

echo "==> database API: scope isolation (other site must not see it)"
other=$(curl -sk --resolve other.spot.localhost:8443:127.0.0.1 \
    https://other.spot.localhost:8443/api/db/entries)
echo "$other" | grep -q "e2e was here" && fail "document leaked across site scopes"

echo "==> database API: delete"
code=$($CURL -o /dev/null -w '%{http_code}' -X DELETE \
    "https://demo.spot.localhost:8443/api/db/entries/$id")
[ "$code" = "204" ] || fail "delete returned $code, want 204"

echo "==> access control: restricted site fails closed, open site stays open"
( cd examples/secret && ../../cli/spot deploy secret ) >/dev/null
ok=""
for _ in $(seq 1 30); do
    code=$(curl -sk --resolve secret.spot.localhost:8443:127.0.0.1 -o /dev/null \
        -w '%{http_code}' https://secret.spot.localhost:8443/ 2>/dev/null || true)
    # 503 means no usable identity resolver; 403 means a resolver is
    # configured and the caller is not allowlisted. Both are fail-closed.
    if [ "$code" = "503" ] || [ "$code" = "403" ]; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "restricted site returned $code, want 403 or 503 (fail closed)"
code=$($CURL -o /dev/null -w '%{http_code}' https://demo.spot.localhost:8443/)
[ "$code" = "200" ] || fail "open site returned $code after access-control deploy, want 200"

echo "==> websocket endpoint is routed through Caddy"
code=$($CURL -o /dev/null -w '%{http_code}' https://demo.spot.localhost:8443/api/ws)
# A plain GET (no Upgrade headers) must reach the handler and be told to
# upgrade — proving the route exists end to end.
[ "$code" = "426" ] || fail "/api/ws returned $code, want 426 Upgrade Required"

echo "==> file uploads: roundtrip through Caddy"
payload="spot e2e payload $$"
printf '%s' "$payload" > /tmp/spot-e2e-upload.txt
uploaded=$($CURL -F "file=@/tmp/spot-e2e-upload.txt" https://demo.spot.localhost:8443/api/files)
url=$(echo "$uploaded" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
[ -n "$url" ] || fail "upload did not return a url: $uploaded"
downloaded=$($CURL "https://demo.spot.localhost:8443$url")
[ "$downloaded" = "$payload" ] || fail "downloaded content differs: $downloaded"
rm -f /tmp/spot-e2e-upload.txt

# The CLI deploys above drew from /api/deploy's per-peer budget (1 per
# 2s, burst 3). On re-runs the wait loops short-circuit (old content is
# already live), so deploys bunch up — refill the bucket between steps
# rather than flaking on the rate limit.
refill_deploy_budget() { sleep 4; }

refill_deploy_budget
echo "==> web deploy: multipart deploy through the apex /api/deploy"
webdir=$(mktemp -d)
printf '<h1>spot web deploy</h1>' > "$webdir/index.html"
printf 'p{color:red}' > "$webdir/app.css"
# Filenames carry site-relative paths, wrapped in a folder like a
# browser folder pick — the API must strip the wrapping.
deployed=$($CURL -F 'site=webdeploy' \
    -F "files=@$webdir/index.html;filename=site/index.html" \
    -F "files=@$webdir/app.css;filename=site/css/app.css" \
    https://spot.localhost:8443/api/deploy)
echo "$deployed" | grep -q '"files":2' || fail "web deploy failed: $deployed"
ok=""
for _ in $(seq 1 30); do
    if curl -sk --resolve webdeploy.spot.localhost:8443:127.0.0.1 \
        https://webdeploy.spot.localhost:8443/ 2>/dev/null | grep -q "spot web deploy"; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "web-deployed site not served at webdeploy.spot.localhost"
css=$(curl -sk --resolve webdeploy.spot.localhost:8443:127.0.0.1 \
    https://webdeploy.spot.localhost:8443/css/app.css)
[ "$css" = "p{color:red}" ] || fail "nested file not served: $css"
echo "    web-deployed site is live"

refill_deploy_budget
echo "==> web deploy: redeploy removes stale files"
deployed=$($CURL -F 'site=webdeploy' \
    -F "files=@$webdir/index.html;filename=index.html" \
    https://spot.localhost:8443/api/deploy)
echo "$deployed" | grep -q '"files":1' || fail "redeploy failed: $deployed"
ok=""
for _ in $(seq 1 30); do
    code=$(curl -sk --resolve webdeploy.spot.localhost:8443:127.0.0.1 -o /dev/null \
        -w '%{http_code}' https://webdeploy.spot.localhost:8443/css/app.css 2>/dev/null || true)
    if [ "$code" = "404" ]; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "stale file still served after redeploy (got $code)"
rm -rf "$webdir"

refill_deploy_budget
echo "==> web deploy: refused from a site subdomain"
code=$(curl -sk --resolve demo.spot.localhost:8443:127.0.0.1 -o /dev/null -w '%{http_code}' \
    -F 'site=demo' -F "files=@scripts/e2e.sh;filename=index.html" \
    https://demo.spot.localhost:8443/api/deploy)
[ "$code" = "400" ] || fail "deploy from site subdomain returned $code, want 400"

echo "==> platform pages: /spots and /gallery are served"
$CURL https://spot.localhost:8443/spots | grep -q "My spots" \
    || fail "/spots does not serve the my-spots page"
$CURL https://spot.localhost:8443/gallery | grep -q "Gallery" \
    || fail "/gallery does not serve the gallery page"

echo "==> 404 pages: apex path, unknown site, missing file on a live site"
code=$($CURL -o /dev/null -w '%{http_code}' https://spot.localhost:8443/no-such-page)
[ "$code" = "404" ] || fail "apex unknown path returned $code, want 404"
$CURL https://spot.localhost:8443/no-such-page | grep -q "wandered off" \
    || fail "apex 404 is not the branded page"
code=$(curl -sk --resolve nowhere.spot.localhost:8443:127.0.0.1 -o /dev/null -w '%{http_code}' \
    https://nowhere.spot.localhost:8443/)
[ "$code" = "404" ] || fail "unknown site returned $code, want 404"
curl -sk --resolve nowhere.spot.localhost:8443:127.0.0.1 https://nowhere.spot.localhost:8443/ \
    | grep -q "wandered off" || fail "unknown-site 404 is not the branded page"
code=$($CURL -o /dev/null -w '%{http_code}' https://demo.spot.localhost:8443/no-such-file.css)
[ "$code" = "404" ] || fail "missing site file returned $code, want 404"
$CURL https://demo.spot.localhost:8443/no-such-file.css | grep -q "wandered off" \
    || fail "missing-file 404 is not the branded page"

echo "==> sites API: mine lists the deployer's sites"
mine=$($CURL https://spot.localhost:8443/api/sites/mine)
echo "$mine" | grep -q '"name":"webdeploy"' || fail "/api/sites/mine missing webdeploy: $mine"

echo "==> sites API: gallery lists public sites, hides restricted ones"
public=$($CURL https://spot.localhost:8443/api/sites/public)
echo "$public" | grep -q '"name":"demo"' || fail "/api/sites/public missing demo: $public"
echo "$public" | grep -q '"name":"secret"' && fail "restricted site leaked into the gallery"

echo "==> sites API: refused from a site subdomain"
code=$(curl -sk --resolve demo.spot.localhost:8443:127.0.0.1 -o /dev/null -w '%{http_code}' \
    https://demo.spot.localhost:8443/api/sites/mine)
[ "$code" = "400" ] || fail "sites API from a site subdomain returned $code, want 400"

refill_deploy_budget
echo "==> sites API: delete removes the site"
code=$($CURL -o /dev/null -w '%{http_code}' -X DELETE https://spot.localhost:8443/api/sites/webdeploy)
[ "$code" = "200" ] || fail "delete webdeploy returned $code, want 200"
mine=$($CURL https://spot.localhost:8443/api/sites/mine)
echo "$mine" | grep -q '"name":"webdeploy"' && fail "webdeploy still listed after delete"
code=$($CURL -o /dev/null -w '%{http_code}' -X DELETE https://spot.localhost:8443/api/sites/webdeploy)
[ "$code" = "404" ] || fail "deleting a missing site returned $code, want 404"
echo "    webdeploy deleted"

echo "==> AI proxy"
ai_body='{"messages":[{"role":"user","content":"Reply with the single word ok"}]}'
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    ai_res=$($CURL -X POST -H 'Content-Type: application/json' -d "$ai_body" \
        https://demo.spot.localhost:8443/api/ai/chat)
    echo "$ai_res" | grep -q '"text"' || fail "AI chat with key did not return text: $ai_res"
    echo "    real Claude API call succeeded"
else
    code=$($CURL -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
        -d "$ai_body" https://demo.spot.localhost:8443/api/ai/chat)
    [ "$code" = "503" ] || fail "AI chat without key returned $code, want 503"
    echo "    no ANTHROPIC_API_KEY in env: 503 fail-loud verified"
fi

echo "==> rate limiting: upload endpoint throttles a parallel burst"
printf 'x' > /tmp/spot-e2e-rl.txt
# Sequential requests refill the bucket between calls; only a parallel
# burst reliably exceeds it (uploads: 2/s, burst 10).
limited=$(seq 1 15 | xargs -P 15 -I{} curl -sk \
    --resolve demo.spot.localhost:8443:127.0.0.1 \
    -o /dev/null -w '%{http_code}\n' -F "file=@/tmp/spot-e2e-rl.txt" \
    https://demo.spot.localhost:8443/api/files | grep -c '^429$' || true)
rm -f /tmp/spot-e2e-rl.txt
[ "${limited:-0}" -ge 1 ] || fail "no 429 across 15 parallel uploads"
echo "    $limited of 15 burst requests were throttled"

echo "==> identity API"
me_file=$(mktemp)
code=$($CURL -o "$me_file" -w '%{http_code}' https://demo.spot.localhost:8443/api/me)
me=$(cat "$me_file")
rm -f "$me_file"
if [ "$code" = "200" ]; then
    echo "$me" | grep -q "\"email\":\"$SPOT_DEV_IDENTITY_EMAIL\"" \
        || fail "/api/me returned unexpected dev identity: $me"
else
    case "$code:$me" in
        503:*"identity resolver not configured"*|404:*"no identity matches"*) ;;
        *) fail "/api/me returned $code $me, want dev identity or fail-loud resolver error" ;;
    esac
fi

echo "==> access directory: apex answers, site subdomain refused"
# The dev static resolver has no NetBird directory, so the list is empty
# — but the endpoint must still answer on the apex and refuse on sites,
# matching /api/deploy's apex-only rule.
sug=$($CURL "https://spot.localhost:8443/api/access/suggestions?q=any")
echo "$sug" | grep -q '"suggestions"' || fail "apex suggestions did not return a list: $sug"
code=$(curl -sk --resolve demo.spot.localhost:8443:127.0.0.1 -o /dev/null -w '%{http_code}' \
    "https://demo.spot.localhost:8443/api/access/suggestions?q=any")
[ "$code" = "400" ] || fail "suggestions from a site subdomain returned $code, want 400"

echo ""
echo "E2E PASS"
