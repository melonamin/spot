#!/bin/sh
# End-to-end test: brings the full stack up, deploys the demo site through
# the real CLI, and exercises serving + the database API through Caddy.
set -eu

cd "$(dirname "$0")/.."

CURL="curl -sk --resolve demo.quick.localhost:8443:127.0.0.1 --resolve quick.localhost:8443:127.0.0.1"

fail() {
    echo "E2E FAIL: $1" >&2
    exit 1
}

echo "==> starting stack"
mkdir -p data/sites
docker compose up -d --build

echo "==> waiting for Caddy and the API"
ready=""
for _ in $(seq 1 60); do
    code=$($CURL -o /dev/null -w '%{http_code}' https://quick.localhost:8443/ 2>/dev/null || true)
    api=$($CURL -o /dev/null -w '%{http_code}' https://demo.quick.localhost:8443/api/me 2>/dev/null || true)
    if [ "$code" = "200" ] && [ "$api" != "000" ] && [ -n "$api" ]; then
        ready=1
        break
    fi
    sleep 1
done
[ -n "$ready" ] || fail "stack did not become ready within 60s"

echo "==> deploying the demo site via the CLI"
( cd examples/demo && ../../cli/quick deploy demo )

echo "==> waiting for the rclone mount to see the deploy"
ok=""
for _ in $(seq 1 30); do
    if $CURL https://demo.quick.localhost:8443/ 2>/dev/null | grep -q "Quick demo guestbook"; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "deployed site not served at demo.quick.localhost"
echo "    site is live"

echo "==> database API: create"
created=$($CURL -X POST -H 'Content-Type: application/json' \
    -d '{"message":"e2e was here"}' \
    https://demo.quick.localhost:8443/api/db/entries)
echo "$created" | grep -q '"id"' || fail "create did not return a document: $created"
id=$(echo "$created" | sed -n 's/.*"id":"\([0-9a-f-]*\)".*/\1/p')

echo "==> database API: list"
$CURL https://demo.quick.localhost:8443/api/db/entries | grep -q "e2e was here" \
    || fail "created document missing from list"

echo "==> database API: scope isolation (other site must not see it)"
other=$(curl -sk --resolve other.quick.localhost:8443:127.0.0.1 \
    https://other.quick.localhost:8443/api/db/entries)
echo "$other" | grep -q "e2e was here" && fail "document leaked across site scopes"

echo "==> database API: delete"
code=$($CURL -o /dev/null -w '%{http_code}' -X DELETE \
    "https://demo.quick.localhost:8443/api/db/entries/$id")
[ "$code" = "204" ] || fail "delete returned $code, want 204"

echo "==> access control: restricted site fails closed, open site stays open"
( cd examples/secret && ../../cli/quick deploy secret ) >/dev/null
ok=""
for _ in $(seq 1 30); do
    code=$(curl -sk --resolve secret.quick.localhost:8443:127.0.0.1 -o /dev/null \
        -w '%{http_code}' https://secret.quick.localhost:8443/ 2>/dev/null || true)
    # 503, not 403: the policy exists but no identity resolver is
    # configured in e2e — restricted sites must fail closed.
    if [ "$code" = "503" ]; then
        ok=1
        break
    fi
    sleep 1
done
[ -n "$ok" ] || fail "restricted site returned $code, want 503 (fail closed)"
code=$($CURL -o /dev/null -w '%{http_code}' https://demo.quick.localhost:8443/)
[ "$code" = "200" ] || fail "open site returned $code after access-control deploy, want 200"

echo "==> websocket endpoint is routed through Caddy"
code=$($CURL -o /dev/null -w '%{http_code}' https://demo.quick.localhost:8443/api/ws)
# A plain GET (no Upgrade headers) must reach the handler and be told to
# upgrade — proving the route exists end to end.
[ "$code" = "426" ] || fail "/api/ws returned $code, want 426 Upgrade Required"

echo "==> file uploads: roundtrip through Caddy"
payload="quick e2e payload $$"
printf '%s' "$payload" > /tmp/quick-e2e-upload.txt
uploaded=$($CURL -F "file=@/tmp/quick-e2e-upload.txt" https://demo.quick.localhost:8443/api/files)
url=$(echo "$uploaded" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
[ -n "$url" ] || fail "upload did not return a url: $uploaded"
downloaded=$($CURL "https://demo.quick.localhost:8443$url")
[ "$downloaded" = "$payload" ] || fail "downloaded content differs: $downloaded"
rm -f /tmp/quick-e2e-upload.txt

echo "==> AI proxy"
ai_body='{"messages":[{"role":"user","content":"Reply with the single word ok"}]}'
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    ai_res=$($CURL -X POST -H 'Content-Type: application/json' -d "$ai_body" \
        https://demo.quick.localhost:8443/api/ai/chat)
    echo "$ai_res" | grep -q '"text"' || fail "AI chat with key did not return text: $ai_res"
    echo "    real Claude API call succeeded"
else
    code=$($CURL -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
        -d "$ai_body" https://demo.quick.localhost:8443/api/ai/chat)
    [ "$code" = "503" ] || fail "AI chat without key returned $code, want 503"
    echo "    no ANTHROPIC_API_KEY in env: 503 fail-loud verified"
fi

echo "==> rate limiting: upload endpoint throttles a parallel burst"
printf 'x' > /tmp/quick-e2e-rl.txt
# Sequential requests refill the bucket between calls; only a parallel
# burst reliably exceeds it (uploads: 2/s, burst 10).
limited=$(seq 1 15 | xargs -P 15 -I{} curl -sk \
    --resolve demo.quick.localhost:8443:127.0.0.1 \
    -o /dev/null -w '%{http_code}\n' -F "file=@/tmp/quick-e2e-rl.txt" \
    https://demo.quick.localhost:8443/api/files | grep -c '^429$' || true)
rm -f /tmp/quick-e2e-rl.txt
[ "${limited:-0}" -ge 1 ] || fail "no 429 across 15 parallel uploads"
echo "    $limited of 15 burst requests were throttled"

echo "==> identity API without NetBird configured fails loudly"
me=$($CURL https://demo.quick.localhost:8443/api/me)
echo "$me" | grep -q "identity resolver not configured" \
    || fail "/api/me did not report missing resolver config: $me"

echo ""
echo "E2E PASS"
