# Spot Architecture

This document gives developers enough system context to change Spot without
accidentally breaking its routing, identity, storage, or security boundaries.

Spot is a small self-hosted hosting platform. The production runtime is centered
on one Go service, `spot-api`, backed by SQLite for metadata and either
S3-compatible object storage or the local filesystem for deployed assets and
uploads.

## System Shape

```text
browser / CLI / deployed site
        |
        | HTTP(S), optional trusted proxy
        v
    spot-api
        |
        |- embedded platform UI and SDK assets
        |- deployed site static serving
        |- deploy, site admin, database, files, realtime, and AI APIs
        |- mesh identity resolution
        |
        |- SQLite metadata: documents, site registry, deploy audit
        |
        `- site and upload storage:
             - S3-compatible storage, usually RustFS locally
             - or local filesystem storage in single-binary mode
```

Important directories:

- `server/`: Go service, routing, identity, deploys, storage adapters, SQLite
  stores, realtime hubs, and tests.
- `sdk/`: browser platform UI and client SDK source files.
- `server/static_assets/sdk/`: generated embedded copies of selected `sdk/`
  files. Regenerate with `go generate` in `server/` after SDK asset changes.
- `cli/spot`: shell CLI used to deploy folders.
- `docker-compose*.yml`, `caddy/`, `scripts/`, `justfile`: local and production
  deployment helpers.
- `examples/`: small deployable example sites.

## Runtime Initialization

The service starts in `server/main.go`.

Startup flow:

1. `loadConfig` reads environment variables and `spot-api serve` flags.
2. `validateDeploymentSafety` rejects unsafe combinations such as shared
   deployments without a mesh identity provider, default RustFS credentials, or
   `SPOT_DEV_IDENTITY_EMAIL` outside `.localhost`.
3. SQLite opens through `openSQLiteDB` in `server/db.go`, which applies the
   embedded `server/schema.sql`.
4. An `IdentityResolver` is selected:
   - `StaticResolver` for `SPOT_AUTH_MODE=single-user` or local dev identity.
   - `NetbirdResolver` for NetBird API configuration.
   - `TailscaleResolver` or `TailscaleOAuthResolver` for Tailscale.
5. Site and file storage are wired:
   - `SiteStore` and `FileStore` for S3-compatible storage.
   - `LocalSiteStore` and `LocalFileStore` for filesystem storage.
6. Optional `AIProxy` is created when `OPENAI_API_KEY` is set.
7. `Server.routes()` builds the HTTP mux and wraps it with forwarded-header and
   host validation.

Most code remains in package `main`. Prefer extending the existing narrow
interfaces (`SiteStorage`, `FileStorage`, `IdentityResolver`,
`DeployAuthorizer`, `SiteAdmin`, `SiteManager`) over adding broad framework
layers.

## Request Routing

Routes are defined in `server/handlers.go`.

Hostnames decide whether a request targets the platform apex or a deployed site:

- Apex: `spot.example.com`
- Site: `<site>.spot.example.com`

`siteFromHost` and `validSpotHost` in `server/scope.go` enforce this rule. Only
one site label is accepted; nested subdomains are not site names.

Request host and scheme are derived by:

- Using `Host` and the TLS state for direct requests.
- Trusting `X-Forwarded-Host` and `X-Forwarded-Proto` only when the socket peer
  is in `SPOT_TRUSTED_PROXIES`.
- Rejecting forwarded headers from untrusted peers.

This is implemented in `server/handlers.go` and `server/proxy.go`.

## Static Serving

Static serving lives in `server/static_assets.go`.

The apex serves embedded platform assets:

- `/` -> `sdk/index.html`
- `/spots` and `/gallery` -> `sdk/spots.html`
- other paths map to embedded files under `server/static_assets/sdk/`

Site subdomains serve deployed files through `SiteStorage`:

- `/spot.js` is always served from embedded SDK assets.
- `/` resolves to `index.html`.
- directory paths resolve to `index.html` under that directory.
- missing files return the embedded `404.html`.

The server reads site files from the storage abstraction rather than exposing
S3 or filesystem paths directly.

The apex also serves site gallery thumbnails, implemented in `server/preview.go`:

- `GET /api/sites/<site>/preview` returns a site's optional
  `_screenshot.{jpg,jpeg,png,webp}` shipped at its deployed root. The leading
  underscore marks it as platform metadata, the same convention as
  `_access.json`.
- Thumbnails are served from the apex origin so the gallery `<img>` never
  depends on the site subdomain's certificate.
- Only open sites get a preview; restricted sites return `404` so a thumbnail
  cannot leak their rendered content.
- The image type is sniffed and anything that is not a raster image is refused,
  so a site cannot run script in the apex origin via a mislabeled screenshot.

## Persistence

SQLite is the only metadata database. The schema lives in `server/schema.sql`,
which `server/db.go` embeds into the binary and applies at startup through
`openSQLiteDB`. Editing `schema.sql` changes the live schema; there is no
separate copy to keep in sync.

Tables:

- `documents`: schemaless JSON documents grouped by `scope` and `collection`.
- `sites`: site ownership records.
- `site_deploy_audit`: deploy and delete audit history.

The document store is implemented in `server/docstore.go`. It stores JSON blobs,
publishes realtime document events after successful mutations, and can purge a
site scope when a site is deleted.

The site registry is implemented in `server/site_registry.go`. It owns:

- first-deploy site claims,
- owner/admin redeploy authorization,
- site listing for the platform UI,
- delete authorization,
- deploy audit writes.

There is no migration framework yet. Schema changes should be additive and
idempotent in `schema.sql`, with focused tests around the new behavior.

## Storage

There are two storage families with the same runtime interfaces.

S3-compatible storage:

- `server/deploy.go`: `SiteStore` stores deployed site files.
- `server/filestore.go`: `FileStore` stores user uploads.
- Buckets are created at startup if missing.
- Browsers never receive storage credentials; all reads and writes go through
  `spot-api`.

Local filesystem storage:

- `server/local_storage.go`: `LocalSiteStore` and `LocalFileStore`.
- Used by `SPOT_STORAGE_MODE=local`.
- Writes are atomic where practical.
- Path validation is strict because site paths become served file paths.

Deploy and upload storage are intentionally separate:

- Deployed site files are the immutable-ish contents of a site deployment.
- Uploads are user-generated files addressed by random IDs under
  `/api/files/<site>/<id>/<name>`.

Deleting a site purges deployed files, uploads, and private document scope
before freeing the site registry row.

## Deploy Flow

Deploy handling is in `server/deploy.go`.

Browser deploys and CLI deploys both submit `POST /api/deploy` on the apex
domain as multipart form data:

- `site`: target site name.
- `files`: one part per file, with the part filename carrying the
  site-relative path.

Deploy invariants:

- Site names must be valid DNS labels.
- Deploys are size-limited and file-count-limited.
- The full deploy is validated before live files are changed. A malformed
  `_access.json` is rejected with `400` at deploy time so a deployer is not
  silently locked out by a policy that fails closed at serve time.
- New files are written before stale files are removed, so a storage failure
  mid-deploy leaves the previous content intact rather than half-applied.
- A per-site mutex prevents concurrent mutations of the same site.
- The first successful deploy claims the site for the actor.
- Later deploys require the same owner or a configured platform admin.
- Sync semantics are used: uploaded files replace the site, and stale files are
  removed.
- Deploy audit rows are recorded for success, failure, and denied attempts.

The deploy API only answers on the apex. Combined with same-origin checks, this
prevents JavaScript running on a deployed site from redeploying another site
using a visitor's ambient mesh identity.

## Identity Model

Spot does not manage browser login sessions. Authentication is ambient network
identity from a mesh, or a configured single-user/static identity.

Core type:

- `Identity` in `server/identity.go`: email, display name, peer name, peer IP,
  and group names.

Resolvers:

- `NetbirdResolver`: maps peer IPs to users and groups from the NetBird API and
  caches results.
- Tailscale resolvers in `server/identity_tailscale.go`: map Tailscale peers to
  identities.
- `StaticResolver`: returns one configured identity for local dev or
  single-user installs.

Handlers call `resolveIdentity` using `clientIP`. If a trusted proxy is in
front, `clientIP` may use trusted forwarded headers; otherwise it uses the
direct peer address.

For shared deployments, host networking or correctly configured trusted proxies
matter because source IP is the credential.

## Access Control

Access control is split by concern.

Request host boundary:

- `rejectUnknownHosts` only accepts the apex, direct site subdomains, and
  `/health`.
- Forwarded headers are accepted only from trusted proxies.

Cross-origin boundary:

- `sameOriginOnly` protects browser APIs that use ambient identity.
- It rejects browser-originated cross-site API calls where `Origin` does not
  match the request host and scheme.

Apex-only APIs:

- Deploy: `POST /api/deploy`
- Site listing, admin, and gallery preview: `/api/sites/*` (including
  `GET /api/sites/{name}/preview`)
- Access suggestions: `/api/access/suggestions`

Site-only APIs:

- Documents: `/api/db/{collection}`
- Files: `/api/files`
- Realtime: `/api/ws`
- AI: `/api/ai/chat`, `/api/ai/image`
- Source download: `/api/download`

Apex-and-site APIs:

- Current identity: `GET /api/me` returns the resolved `Identity`.
- Access check: `GET /api/authz` is a lightweight preflight. On the apex it
  returns `200`; on a site subdomain it applies the site's access policy and
  returns `200` only when the caller is allowed.

Site visitor access:

- `_access.json` at the deployed site root defines an `AccessPolicy`.
- Missing `_access.json` means the site is open to everyone who can reach Spot.
- `allow` present means access is restricted.
- Email entries match users; non-email entries match group names.
- Empty `allow` denies everyone.
- Invalid or unreadable policy files fail closed.
- `download: false` disables source ZIP downloads.
- `ai: "visitors"` allows visitors to use AI when deployment-level AI access is
  otherwise owner-only.

Policy parsing and caching are in `server/policy.go`; storage-backed lookup is
in `server/policy_resolve.go`.

Preserve fail-closed behavior when changing policy or identity code.

## Data Scoping

Database and realtime room scoping live in `server/scope.go`.

Rules:

- APIs must be called from a site subdomain.
- Collection and room names must match `^[a-z0-9_-]{1,64}$`.
- Normal collections and rooms are scoped to the site name.
- Names beginning with `shared-` use the global `_shared` scope.

The underscore in `_shared` is intentional: site names are DNS labels and cannot
contain underscores, so deployed sites cannot forge that scope by hostname.

The browser-visible effect is:

- `posts` on `a.spot.example.com` and `posts` on `b.spot.example.com` are
  separate.
- `shared-posts` on both sites points at the same global collection.

## Realtime

Realtime code is in `server/realtime.go` and `handleWS` in
`server/handlers.go`.

There are two realtime mechanisms:

- Document subscriptions: `DocStore` publishes create/update/delete events to
  `Hub`, keyed by scope and collection.
- Ephemeral rooms: `RoomHub` handles room membership, presence, and room
  messages.

Realtime state is process-local:

- Document state is persisted in SQLite, but event delivery is not durable.
- Room messages and presence are transient and are not replayed after reconnect.

Slow websocket consumers lose events rather than blocking fan-out.

## File Uploads and Downloads

Uploads are implemented in `server/filestore.go` and `handleUpload`.

Upload rules:

- Uploads must originate from a site subdomain.
- Site access is checked before accepting upload or download requests.
- The server sniffs content type instead of trusting the client's declaration.
- Stored upload names are sanitized base names.
- Upload IDs are random 16-byte hex strings.

Download hardening:

- `X-Content-Type-Options: nosniff`.
- A restrictive sandbox CSP.
- Only known safe media types render inline.
- HTML, SVG, unknown types, and other script-capable content download as
  attachments.

Site source downloads are implemented in `server/download.go` and return ZIPs
only when site access allows it and policy does not disable downloads.

## AI Proxy

The AI API is implemented in `server/ai.go`.

`POST /api/ai/chat` lets deployed sites call an OpenAI-compatible chat API
through the server-side key. `POST /api/ai/image` lets deployed sites generate
images through the same OpenAI-compatible gateway.

Access rules:

- Requires a site subdomain.
- Requires normal site access.
- If `SPOT_AI_ACCESS=visitors`, any authorized visitor may use it.
- Otherwise, only site owners/admins may use it unless the site's
  `_access.json` opts into `ai: "visitors"`.
- Requested models must be in the deployment's allowed model set.

The AI proxy is optional. Without `OPENAI_API_KEY`, `/api/ai/chat` and
`/api/ai/image` return `503`. Chat calls use `/v1/chat/completions`; image
calls use `/v1/images/generations` and normalize returned base64 images into
browser-ready `data_url` values. The built-in image allowlist includes
`gpt-image-2` and `gemini-3.1-flash-image`; deployments can point
`OPENAI_BASE_URL` at LiteLLM and map those names or local aliases to provider
routes there.

## Client SDK and Platform UI

The browser-facing files live in `sdk/`.

Common files:

- `sdk/index.html`: apex deploy UI.
- `sdk/spots.html`: gallery/site listing UI.
- `sdk/spot.js`: browser SDK used by deployed sites.
- `sdk/spot`: CLI helper installed by `install.sh`.
- `sdk/agent.md`: agent-facing instructions distributed by Spot.

Embedded copies live in `server/static_assets/sdk/` and are compiled into the Go
binary. After editing SDK assets in `sdk/`, regenerate the embedded copies:

```sh
just generate
```

`just check-generate` regenerates and then fails if the embedded copies have
drifted from `sdk/`, so stale embedded assets are caught instead of shipped.

Then run tests from the repository root with:

```sh
just test
```

## Development Map

Use this map when deciding where a change belongs:

| Change | Start here |
| --- | --- |
| HTTP route or middleware | `server/handlers.go` |
| Apex/static/platform asset serving | `server/static_assets.go`, `sdk/` |
| Site gallery thumbnails | `server/preview.go` |
| Site path rules or data scope rules | `server/scope.go` |
| Deploy validation, sync, limits | `server/deploy.go` |
| Site ownership, admin deploy rights, audit | `server/site_registry.go` |
| Site listing/delete HTTP handlers | `server/sites.go` |
| SQLite open, schema, startup | `server/db.go`, `server/schema.sql` |
| SQLite document behavior | `server/docstore.go`, `server/schema.sql` |
| S3 file uploads | `server/filestore.go` |
| Local filesystem storage | `server/local_storage.go` |
| Source ZIP download | `server/download.go` |
| Mesh identity | `server/identity.go`, `server/identity_tailscale.go` |
| `_access.json` behavior | `server/policy.go`, `server/policy_resolve.go` |
| Realtime document events or rooms | `server/realtime.go`, websocket handling in `server/handlers.go` |
| AI text and image proxy | `server/ai.go` |
| Rate limits | `server/ratelimit.go`, route wiring in `server/handlers.go` |
| Config and startup wiring | `server/main.go` |
| Docker/Caddy deployment shape | `docker-compose*.yml`, `caddy/` |
| CLI deployment behavior | `cli/spot` |

## Testing Expectations

Default verification:

```sh
just test
```

This runs `go vet ./...` and `go test ./...` in `server/`.

Use broader tests when changing behavior across boundaries:

- `just test-integration`: SQLite/storage behavior behind the integration tag.
- `just e2e`: compose stack, demo deploy, static serving, and API smoke tests.
- `just deploy-demo`: quick manual local deploy after `just up`.

Security-sensitive changes should cover allowed and denied paths. This includes
deploy authorization, `_access.json`, source IP handling, trusted proxies,
same-origin checks, site deletion, uploads/downloads, AI access, and shared data
scope behavior.

## Design Constraints to Preserve

- The mesh source IP is security-sensitive. Do not trust forwarded headers from
  arbitrary callers.
- Deployed site JavaScript must not be able to spend a visitor's identity on
  apex-only administrative APIs.
- Invalid or unreadable `_access.json` must deny access, not open access.
- Site storage credentials stay server-side.
- SQLite remains the source of truth for metadata and site ownership.
- Site names are hostnames; keep DNS-label validation strict.
- Normal data is site-private by default; shared data must remain explicit via
  `shared-*`.
- Deleting a site must not let a future owner inherit old files, uploads, or
  private documents.
- Keep interfaces small and test local behavior at the file that owns it.
