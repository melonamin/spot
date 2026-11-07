# Spot: internal hosting platform prototype.

default:
    @just --list

# Bring the local stack up (spot-api on http://*.spot.localhost:8080).
up:
    docker compose up -d --build

down:
    docker compose down

# Tear down including data volumes.
nuke:
    docker compose down -v

logs *args:
    docker compose logs -f {{args}}

build:
    cd server && go build ./...

build-binary:
    cd server && go build -o spot-api .

# Unit tests (no external services needed).
test:
    cd server && go vet ./... && go test ./...
    ./scripts/cli-smoke.sh

# Integration tests need SQLite and (for the filestore tests) a running
# S3/RustFS endpoint.
test-integration:
    cd server && go test -tags integration ./...

# Opt-in destructive Cloudflare contract test. Requires dedicated
# SPOT_CLOUDFLARE_* credentials and always removes its project/domain/DNS row.
# Set SPOT_CLOUDFLARE_LIVE_TEST_EMAIL to also exercise Access restriction and
# the return-to-public transition.
test-cloudflare-live:
    cd server && SPOT_CLOUDFLARE_LIVE_TEST=1 go test -count=1 -tags=integration,cloudflarelive -run '^TestCloudflareLivePublishRoundTrip$' .

# Regenerate the built-in default icons from their SVG sources (needs rsvg-convert and ImageMagick).
default-icons:
    #!/bin/sh
    set -eu
    cd server/default_icons
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    for size in 16 32 48; do rsvg-convert -w "$size" -h "$size" favicon.svg -o "$tmp/$size.png"; done
    magick "$tmp/16.png" "$tmp/32.png" "$tmp/48.png" favicon.ico
    rsvg-convert -w 180 -h 180 apple-touch-icon.svg -o "$tmp/touch.png"
    magick "$tmp/touch.png" -background '#5a4bf0' -alpha remove -alpha off -strip PNG24:apple-touch-icon.png

# Sync the embedded SDK copy under server/static_assets/sdk.
generate:
    cd server && go generate ./...

# Fail if the embedded SDK copy is stale (drift between sdk/ and
# server/static_assets/sdk/).
check-generate:
    sh -ec 'before=$(mktemp); after=$(mktemp); trap "rm -f $before $after" EXIT; git diff -- server/static_assets/sdk > "$before"; cd server; go generate ./...; cd ..; git diff -- server/static_assets/sdk > "$after"; diff -u "$before" "$after"'

# Full end-to-end: stack up, deploy demo site, exercise serving + DB API.
e2e:
    ./scripts/e2e.sh

# Browser SDK smoke: starts a local Spot server with a fake AI gateway and
# exercises sdk/spot.js through its public methods.
sdk-smoke:
    node scripts/sdk-smoke.mjs

# Deploy the committed tree to production.
deploy *args:
    ./scripts/deploy-prod.sh {{args}}

# Deploy the demo guestbook to the local stack.
deploy-demo:
    cd examples/demo && ../../cli/spot deploy demo

# Deploy the Spot Show example to the local stack.
deploy-show-demo:
    SPOT_URL=http://spot.localhost:8080 ./cli/spot show deploy spot-show examples/spot-show/show.json
