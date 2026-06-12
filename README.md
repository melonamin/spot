# Spot

Drop a folder, get a spot.

Spot is a self-hosted internal hosting platform inspired by
[Shopify's Quick](https://shopify.engineering/quick): drop a folder of
HTML, get a site on the internal network. No frameworks, no pipelines,
no per-site config.

```
employee device (NetBird peer)
        │  wireguard mesh — NetBird policy decides who reaches the VM
        ▼
spot VM (NetBird peer)
  ├─ Caddy        wildcard *.spot.<domain>: site files + /api + /spot.js
  ├─ spot-api    Go: document DB, identity from NetBird peer IPs
  ├─ Postgres     JSONB document store
  ├─ RustFS       S3 buckets spot-sites / spot-uploads
  └─ rclone       FUSE-mounts spot-sites read-only for Caddy
```

Authentication is the mesh itself: only NetBird peers can reach the VM,
and WireGuard source IPs are cryptographically bound to peers. Identity
(`/api/me`) is resolved by mapping the request's peer IP to its owner via
the NetBird management API — no cookies, no OIDC redirects.

## Run it

```sh
just up        # full stack on https://*.spot.localhost:8443
just e2e       # end-to-end: deploy demo site, exercise serving + DB API
```

Deploy any folder with an `index.html`:

```sh
cli/spot deploy mysite     # -> https://mysite.spot.localhost:8443/
```

Or skip the terminal: the apex page (`https://spot.localhost:8443/`) is
a deployer — drop a folder on it, pick a name, hit Launch. It posts the
files to `POST /api/deploy` on the apex, which syncs them into the same
`spot-sites` bucket the CLI uses (stale files are removed, like `rclone
sync`). The endpoint only answers on the apex host, so a deployed
site's JavaScript can never redeploy other sites with a visitor's
browser.

`cli/spot init` writes an agent skill into the current project so coding
agents know the SDK without reading docs.

## Tests

```sh
just test               # unit
just test-integration   # against the compose PostgreSQL (needs `just up`)
just e2e                # full stack through Caddy
```

## SDK

Sites load `/spot.js` from their own origin (Caddy serves it on every
host, and routes `/api/*` to the shared backend — same-origin, no CORS):

```js
const me = await spot.me();                      // { email, name, peer_name, peer_ip, groups }
const posts = spot.db.collection('posts');
await posts.create({ title: 'Hello Spot DB' });
await posts.list();
```

Collections are private to their site, with one exception: collections
named `shared-*` live in a single global namespace that every site can
read and write — that's how cross-site libraries (leaderboards,
comments, analytics) share data. The prefix makes sharing an explicit,
visible choice.

Realtime: any collection can be subscribed to, and every visitor sees
changes live. Under the hood each write fires `pg_notify` in its own
transaction; a dedicated listener connection relays events to a hub
that fans out to websocket sessions (`/api/ws`):

```js
const unsubscribe = posts.subscribe({
  onCreate: (doc) => ...,
  onUpdate: (doc) => ...,
  onDelete: (id) => ...,
});
```

A consequence of the same-origin routing: sites cannot serve their own
files under `/api/` or at `/spot.js`.

## Access control

Sites are **open to everyone on the mesh by default**. A site restricts
itself by shipping an `_access.json` at its root:

```json
{ "allow": ["alice@corp.com", "team-payments"] }
```

Entries containing `@` match the visitor's email; everything else
matches a NetBird group name (device groups and the owner's
auto-groups). Caddy consults `spot-api` (`forward_auth` → `/api/authz`)
on every request, so the policy covers static files and the site's
database API alike. Restricted sites **fail closed**: an unparseable
policy or an unreachable identity resolver denies access rather than
allowing it.

Two consequences to be aware of:

- The policy protects *visibility*, not *integrity*: Spot has no site
  ownership, so anyone on the mesh can redeploy a site, including its
  `_access.json`. If a site ever needs real ownership, that requires
  per-site deploy credentials — deliberately not built.
- `_access.json` is an allowlist, not a secret; permitted visitors can
  fetch it like any other file of the site.

## Production notes

- **DNS**: publish `*.spot.<domain>` as an A record pointing at the VM's
  NetBird IP. Off-mesh clients can resolve it but cannot route to it.
- **Source IP must be the peer IP** — identity resolves the request's
  source address against the NetBird peer list, so the front proxy has to
  see the real mesh IP. Docker bridge port-publishing SNATs every inbound
  connection to the docker gateway, which would make every visitor look
  like one non-peer address and break identity. Run Caddy on the host
  network: `docker compose -f docker-compose.yml -f docker-compose.netbird.yml up -d`
  (see `docker-compose.netbird.yml`). Without this, `/api/me` returns
  404 for everyone.
- **TLS**: replace `tls internal` in `caddy/Caddyfile` with a DNS-01
  wildcard challenge (e.g. the caddy-dns/cloudflare module) for publicly
  trusted certs. Also bump the `{labels.N}` index to match your domain's
  label count (see the comment in the Caddyfile).
- **NetBird**: create an access policy `employees -> spot-vm:443`, and a
  service-account PAT for `NETBIRD_API_URL`/`NETBIRD_API_TOKEN` so the
  API can resolve peer IPs to users.
- **RustFS** is alpha (v1.0.0-alpha, single-node). Sites are regenerable,
  so the blast radius is low; Garage or SeaweedFS are drop-in S3
  alternatives if that bothers you.
- Like the original, there are no site owners and no permissions: anyone
  on the mesh can overwrite any site. That is the point.

## Files and AI

Uploads go through the server (browsers never see storage credentials)
into the `spot-uploads` bucket; download URLs are immutable and work
from any site:

```js
const stored = await spot.files.upload(file);  // { id, name, size, content_type, url }
```

The content type is sniffed from the bytes, not the client's claim.
Images, PDFs, plain text, and audio/video render inline; everything else
(HTML, SVG, unknown) is served as a sandboxed `attachment` with
`nosniff` so an uploaded file can't run script in a viewer's site origin.
For stricter isolation in production, serve `/api/files` from a separate
cookieless domain.

The AI proxy holds the Anthropic key server-side (`ANTHROPIC_API_KEY`
in `.env`); sites call Claude with zero configuration. Defaults:
`claude-opus-4-8`, adaptive thinking, 16K max tokens — overridable per
request with `model`, `system`, `max_tokens`:

```js
const res = await spot.ai.chat([{ role: 'user', content: 'Summarize my tasks' }]);
console.log(res.text);
```

## Rate limits

Per peer IP: database 25 req/s (burst 50), uploads 2 req/s (burst 10),
AI 1 request per 2s (burst 10), deploys 1 per 2s (burst 3). The authz
endpoint Caddy consults for static files is deliberately unlimited.

Compared to the blog's feature list, only the data warehouse
(Shopify-specific BigQuery) is deliberately omitted — wire your own
warehouse in if one exists.
