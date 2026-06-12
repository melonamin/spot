# Spot: internal hosting platform prototype.

default:
    @just --list

# Bring the whole stack up (Caddy on https://*.spot.localhost:8443).
up:
    mkdir -p data/sites
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

# Unit tests (no external services needed).
test:
    cd server && go vet ./... && go test ./...

# Integration tests against the compose PostgreSQL (run `just up` first).
test-integration:
    cd server && go test -tags integration ./...

# Full end-to-end: stack up, deploy demo site, exercise serving + DB API.
e2e:
    ./scripts/e2e.sh

# Deploy the committed tree to production.
deploy *args:
    ./scripts/deploy-prod.sh {{args}}

# Deploy the demo guestbook to the local stack.
deploy-demo:
    cd examples/demo && ../../cli/spot deploy demo
