# Quick

A self-hosted clone of [Shopify's Quick](https://shopify.engineering/quick):
drop a folder of HTML, get a site on the internal network. No frameworks,
no pipelines, no per-site config.

```
employee device (NetBird peer)
        │  wireguard mesh — NetBird policy decides who reaches the VM
        ▼
quick VM (NetBird peer)
  ├─ Caddy        wildcard *.quick.<domain>: site files + /api + /quick.js
  ├─ quick-api    Go: document DB, identity from NetBird peer IPs
  ├─ Postgres     JSONB document store
  ├─ RustFS       S3 buckets quick-sites / quick-uploads
  └─ rclone       FUSE-mounts quick-sites read-only for Caddy
```

Authentication is the mesh itself: only NetBird peers can reach the VM,
and WireGuard source IPs are cryptographically bound to peers. Identity
(`/api/me`) is resolved by mapping the request's peer IP to its owner via
the NetBird management API — no cookies, no OIDC redirects.

## Run it

```sh
just up        # full stack on https://*.quick.localhost:8443
just e2e       # end-to-end: deploy demo site, exercise serving + DB API
```

Deploy any folder with an `index.html`:

```sh
cli/quick deploy mysite     # -> https://mysite.quick.localhost:8443/
```

`cli/quick init` writes an agent skill into the current project so coding
agents know the SDK without reading docs.

## Tests

```sh
just test               # unit
just test-integration   # against the compose PostgreSQL (needs `just up`)
just e2e                # full stack through Caddy
```

## SDK

Sites load `/quick.js` from their own origin (Caddy serves it on every
host, and routes `/api/*` to the shared backend — same-origin, no CORS):

```js
const me = await quick.me();                      // { email, name, peer_name, peer_ip, groups }
const posts = quick.db.collection('posts');
await posts.create({ title: 'Hello Quick DB' });
await posts.list();
```

Collections are private to their site, with one exception: collections
named `shared-*` live in a single global namespace that every site can
read and write — that's how cross-site libraries (leaderboards,
comments, analytics) share data. The prefix makes sharing an explicit,
visible choice.

A consequence of the same-origin routing: sites cannot serve their own
files under `/api/` or at `/quick.js`.

## Access control

Sites are **open to everyone on the mesh by default**. A site restricts
itself by shipping an `_access.json` at its root:

```json
{ "allow": ["alice@corp.com", "team-payments"] }
```

Entries containing `@` match the visitor's email; everything else
matches a NetBird group name (device groups and the owner's
auto-groups). Caddy consults `quick-api` (`forward_auth` → `/api/authz`)
on every request, so the policy covers static files and the site's
database API alike. Restricted sites **fail closed**: an unparseable
policy or an unreachable identity resolver denies access rather than
allowing it.

Two consequences to be aware of:

- The policy protects *visibility*, not *integrity*: Quick has no site
  ownership, so anyone on the mesh can redeploy a site, including its
  `_access.json`. If a site ever needs real ownership, that requires
  per-site deploy credentials — deliberately not built.
- `_access.json` is an allowlist, not a secret; permitted visitors can
  fetch it like any other file of the site.

## Production notes

- **DNS**: publish `*.quick.<domain>` as an A record pointing at the VM's
  NetBird IP (100.64.0.0/10). Off-mesh clients can resolve it but cannot
  route to it.
- **TLS**: replace `tls internal` in `caddy/Caddyfile` with a DNS-01
  wildcard challenge (e.g. the caddy-dns/cloudflare module) for publicly
  trusted certs. Also bump the `{labels.N}` index to match your domain's
  label count (see the comment in the Caddyfile).
- **NetBird**: create an access policy `employees -> quick-vm:443`, and a
  service-account PAT for `NETBIRD_API_URL`/`NETBIRD_API_TOKEN` so the
  API can resolve peer IPs to users.
- **RustFS** is alpha (v1.0.0-alpha, single-node). Sites are regenerable,
  so the blast radius is low; Garage or SeaweedFS are drop-in S3
  alternatives if that bothers you.
- Like the original, there are no site owners and no permissions: anyone
  on the mesh can overwrite any site. That is the point.

## Not built yet (in blog-post order)

- realtime subscriptions on the document store (Postgres LISTEN/NOTIFY
  is already a natural fit)
- file uploads (presigned URLs against the quick-uploads bucket)
- AI proxy (`quick.ai.chat()` with server-side keys)
- websocket rooms for multiplayer
- rate limiting (the blog learned this the hard way)
