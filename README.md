# Spot

Drop a folder, get a spot.

![Spot deploy UI](docs/assets/spot-home.png)

Spot is a self-hosted internal hosting platform for small web things that
should be easy to ship and private by default. Put it on a VM inside your
NetBird or Tailscale mesh, drop a folder of HTML from the browser or CLI,
and get a private URL like `http://demo.spot.example.com`.

For trusted homelabs, Spot can also run in `single-user` mode: no provider
API, no per-person identity, just one fixed owner for everyone who can
reach the box.

## How It Works

```text
mesh peer
   |
   | WireGuard mesh decides who can reach the VM
   v
spot VM
  |- spot-api   Go binary: platform UI, deployed sites, APIs, realtime
  |- SQLite     metadata, documents, deploy registry, audit log
  `- S3/RustFS  deployed site files and uploads
```

Authentication is the mesh itself. NetBird or Tailscale decides who can
reach the VM, and Spot maps the request's mesh source IP to an identity
through the provider API. There are no cookies, sessions, OIDC redirects,
or per-site deploy keys.

S3-compatible storage stays in the architecture because deployed sites
and uploads can be large and are the easiest part to keep in blob storage.
SQLite is the only metadata database.

## Quick Start

```sh
cp .env.example .env
just up
```

The local compose stack runs:

- `spot-api` on `http://spot.localhost:8080`
- RustFS on loopback for S3-compatible blob storage
- SQLite in the `spot-data` volume

Local development uses `SPOT_DEV_IDENTITY_EMAIL=dev@spot.local` by
default so deploys work without a mesh API. Shared deployments must use a
real mesh provider or `SPOT_AUTH_MODE=single-user`.

Deploy a folder with an `index.html`:

```sh
cli/spot deploy demo examples/demo
```

Then open:

```text
http://demo.spot.localhost:8080/
```

The CLI targets `SPOT_URL` and defaults to
`http://spot.localhost:8080`. Persist another deployment with:

```sh
mkdir -p ~/.config/spot
printf 'SPOT_URL=https://spot.example.com\n' > ~/.config/spot/env
```

The apex page is also a deployer: open `http://spot.localhost:8080/`,
drop a folder or `index.html`, pick a name, and launch.

## Deployment Modes

### Prebuilt Images

Multi-architecture images (`linux/amd64` and `linux/arm64`) are published to
the GitHub Container Registry:

- `ghcr.io/melonamin/spot-api` — the Spot server.
- `ghcr.io/melonamin/spot-caddy` — Caddy with the Cloudflare DNS module, for
  `SPOT_TLS_MODE=tls-cloudflare`.

Available tags:

- `latest` — the most recent tagged release.
- `X.Y.Z` and `X.Y` — a specific release.
- `edge` — the current `main` branch.
- `sha-<commit>` — a specific commit.

The Compose files reference these images by default, so the commands below
pull a prebuilt image when you omit `--build`. Images are published starting
with the first tagged release; before a release exists, or to run unreleased
code, build from source by adding `--build`. To pin a release, set
`SPOT_API_IMAGE` (and `SPOT_CADDY_IMAGE` for the TLS overlay):

```sh
SPOT_API_IMAGE=ghcr.io/melonamin/spot-api:0.1.0 \
  docker compose -f docker-compose.yml -f docker-compose.mesh.yml up -d
```

### Mesh Identity

Use this for the normal shared deployment model.

1. Point the apex and wildcard DNS at the VM's mesh IP:

   ```text
   spot.example.com      A/AAAA  <vm mesh ip>
   *.spot.example.com    A/AAAA  <vm mesh ip>
   ```

2. Set a non-local domain and replace default RustFS credentials:

   ```env
   SPOT_MESH_DOMAIN=spot.example.com
   RUSTFS_ACCESS_KEY=...
   RUSTFS_SECRET_KEY=...
   ```

3. Configure exactly one provider:

   ```env
   NETBIRD_API_URL=https://netbird.example.com
   NETBIRD_API_TOKEN=...
   ```

   or:

   ```env
   TAILSCALE_OAUTH_CLIENT_ID=...
   TAILSCALE_OAUTH_CLIENT_SECRET=...
   TAILSCALE_TAILNET=-
   ```

4. Start with host networking for `spot-api`, so it sees the real mesh
   peer source IP:

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.mesh.yml up -d --build
   ```

To let Spot serve HTTPS directly, add the TLS overlay:

```sh
docker compose \
  -f docker-compose.yml \
  -f docker-compose.mesh.yml \
  -f docker-compose.tls.yml \
  up -d --build
```

`SPOT_TLS_MODE=tls-internal` uses Caddy's internal CA.
`SPOT_TLS_MODE=tls-cloudflare` uses DNS-01 wildcard certificates and
requires `CF_API_TOKEN` with Zone:Zone:Read and Zone:DNS:Edit.

If you put another TLS proxy in front, preserve the real source IP and
add only that proxy to `SPOT_TRUSTED_PROXIES` so Spot can trust
`X-Forwarded-Proto` and `X-Forwarded-For`.

### Single-User Homelab

Use this when LAN/VPN/firewall access is the boundary and everyone who
can reach Spot should act as the same owner:

```sh
docker compose -f docker-compose.yml -f docker-compose.homelab.yml up -d --build
```

Set `SPOT_HOMELAB_DOMAIN` if you do not want the default
`spot.home.arpa`, and publish the apex plus wildcard to the host's LAN or
VPN IP.

Keep `SPOT_SINGLE_USER_EMAIL` stable. It is the owner key used for deploy
authorization.

### Single Binary, Local Storage

This is the smallest install. It uses SQLite plus filesystem storage, no
RustFS/S3:

```sh
cd server
go build -o spot-api .

./spot-api serve \
  --storage local \
  --auth single-user \
  --domain spot.home.arpa \
  --data-dir /var/lib/spot \
  --listen :8080
```

Data lands under:

```text
/var/lib/spot/
  spot.db
  sites/<site>/...
  uploads/<site>/<id>/<name>
```

Point both DNS records at the machine:

```text
spot.home.arpa      A/AAAA  <vm lan or vpn ip>
*.spot.home.arpa    A/AAAA  <vm lan or vpn ip>
```

Open `http://spot.home.arpa:8080/` unless you put TLS in front.

### Single Binary, S3 Storage

This keeps the one-process Spot runtime while storing large files in
S3-compatible blob storage:

```sh
./spot-api serve \
  --storage s3 \
  --auth single-user \
  --domain spot.home.arpa \
  --data-dir /var/lib/spot \
  --listen :8080
```

Required environment:

```env
SPOT_S3_ENDPOINT=127.0.0.1:9000
SPOT_S3_ACCESS_KEY=...
SPOT_S3_SECRET_KEY=...
SPOT_UPLOADS_BUCKET=spot-uploads
SPOT_SITES_BUCKET=spot-sites
```

The server creates the buckets if they are missing.

## Configuration

Environment variables and CLI flags overlap for the install-critical
settings:

```text
--storage   SPOT_STORAGE_MODE      s3 or local
--auth      SPOT_AUTH_MODE         auto or single-user
--domain    SPOT_DOMAIN
--data-dir  SPOT_DATA_DIR
--sqlite    SPOT_SQLITE_PATH
--listen    PORT
```

`SPOT_STORAGE_MODE` defaults to `s3`. `SPOT_SQLITE_PATH` defaults to
`$SPOT_DATA_DIR/spot.db`.

Spot derives generated URLs from the request. Direct HTTP returns
`http://`, direct TLS returns `https://`, and trusted proxies may send
`X-Forwarded-Proto`. There is no configured public scheme.

## Production Deploy

Deploy the committed tree with the TLS overlay and orphan cleanup:

```sh
scripts/deploy-prod.sh
```

## SDK

Sites load `/spot.js` from their own origin:

```html
<script src="/spot.js"></script>
```

All calls are same-origin:

```js
const me = await spot.me();
const posts = spot.db.collection('posts');
await posts.create({ title: 'Hello Spot DB' });
const docs = await posts.list();
const mine = await posts.list({ mine: true });
```

Collections are private to their site, except `shared-*` collections,
which live in one global namespace every site can read and write.

Every document records an `owner` — the mesh identity that created it — so
several visitors can keep private records in one shared site.
`list({ mine: true })` returns only the caller's documents. Writes are not
restricted to the owner; enforce per-user editing in site logic when needed.

Realtime DB subscriptions are process-local and delivered after SQLite
commits:

```js
const unsubscribe = posts.subscribe({
  onCreate: (doc) => console.log(doc),
  onUpdate: (doc) => console.log(doc),
  onDelete: (id) => console.log(id),
});
```

Ephemeral realtime rooms are also process-local and are not persisted:

```js
const room = spot.realtime.room('control');
room.on('cursor', ({ from, data }) => drawCursor(from.email, data));
room.onPresence((users) => renderOnline(users));
room.setPresence({ role: 'operator' });
room.send('cursor', { x: 12, y: 8 });
```

## Access Control

Sites are open to everyone who can reach Spot by default. A site restricts
itself by shipping `_access.json` at its root:

```json
{ "allow": ["alice@example.com", "team-payments"] }
```

Entries containing `@` match email. Other entries match mesh groups. A
broken policy fails closed.

The first deploy claims a site name. Later deploys and deletes require
the original owner or a platform admin from `SPOT_ADMIN_EMAILS` or
`SPOT_ADMIN_GROUPS`.

In `single-user` mode, every visitor has the same configured identity.
Ownership still works, but `_access.json` cannot provide per-person
authorization.

Sites can disable source downloads without becoming private:

```json
{ "download": false }
```

## Platform Pages

- `/` is the browser deployer.
- `/spots` lists the caller's sites.
- `/gallery` lists unrestricted public sites.

Important APIs:

- `POST /api/deploy` deploys a site from the apex only.
- `GET /api/sites/mine` lists the caller's sites.
- `GET /api/sites/public` lists unrestricted sites.
- `DELETE /api/sites/{name}` deletes a site, its uploads, private docs,
  and registry claim.
- `GET /api/download` on a site subdomain downloads a source ZIP,
  unless the site disables downloads.

## Files, Text AI, and Image Generation

Uploads go through Spot, so browsers never see storage credentials:

```js
const stored = await spot.files.upload(file);
const files = await spot.files.list();
await spot.files.delete(stored);
```

Images, PDFs, plain text, audio, and video render inline. HTML, SVG, and
unknown types download as attachments with `nosniff`.

The AI proxy holds the OpenAI-compatible gateway key server-side. For LiteLLM,
use the LiteLLM virtual key and proxy URL:

```env
OPENAI_API_KEY=...
OPENAI_BASE_URL=http://litellm:4000
SPOT_AI_MODEL=...
SPOT_AI_IMAGE_MODEL=gemini-3.1-flash-image-preview
```

```js
const chat = await spot.ai.chat([{ role: 'user', content: 'Summarize my tasks' }]);

// Stream tokens as they arrive:
await spot.ai.stream([{ role: 'user', content: 'Write a haiku' }], {
  onToken: (delta, text) => render(text),
});

const art = await spot.ai.image('A tiny cyberpunk greenhouse at night');
const img = new Image();
img.src = art.images[0].data_url;
document.body.append(img);
```

Text generation goes through `/v1/chat/completions`; image generation goes
through `/v1/images/generations`. Image responses include browser-ready
`images[0].data_url` plus `b64`, `mime_type`, and `model`. Set
`SPOT_AI_IMAGE_MODEL` to choose the deployment default, or pass a model such
as `{ model: 'gpt-image-2' }` or the LiteLLM Nano Banana 2 alias exposed by
your gateway.

By default only the site owner and platform admins may call it. Set
`SPOT_AI_ACCESS=visitors` globally, or opt in a restricted site:

```json
{ "allow": ["team-payments"], "ai": "visitors" }
```

## Tests

```sh
just test
just test-integration
just e2e
```

`just e2e` starts compose, deploys the demo site, exercises static
serving, DB APIs, uploads, site deletion, and platform pages.

## Production Notes

- Mesh identity depends on the real peer IP. Run Spot directly on the
  host network or behind a proxy that preserves source IP correctly.
- Shared deployments must configure exactly one mesh provider unless
  `SPOT_AUTH_MODE=single-user` is set.
- `SPOT_DEV_IDENTITY_EMAIL` is accepted only for `.localhost`.
- Shared mesh deployments must replace default RustFS credentials.
- RustFS is convenient for local and small deployments. Any
  S3-compatible store can replace it.
- Multi-process Spot against one SQLite file is intentionally not the
  target. If that becomes necessary, add a SQLite pub/sub layer and
  explicit leader/runtime coordination then.
