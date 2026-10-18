# Site types

A site type is an admin-uploaded, immutable viewer package. A typed site is an
ordinary Spot site whose registry row pins a type and an exact version: on the
site's origin Spot serves the type's `index.html` at `/` and its other files
under `/_type/`, and every other path is the site's own data, which a typed
deploy may write only under the type's declared data paths.

This document is the contract for package authors, API clients, and viewer
authors. Spot ships no types; `examples/types/card/` is a small example.

## Package

A package is a directory:

- `type.json`: the manifest (required).
- `index.html`: the viewer entry (required).
- `SKILL.md`: optional instructions for agents. Spot stores and serves it on
  the platform API, never on a site origin, and never interprets it.
- any other viewer files (scripts, styles, fonts, images).

### `type.json`

```json
{
  "format": 1,
  "name": "card",
  "version": "1.0.0",
  "title": "Card",
  "description": "Renders one JSON card.",
  "data": ["card.json", "media/"]
}
```

| Field | Rule |
| --- | --- |
| `format` | Integer `1`. |
| `name` | DNS label: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`. |
| `version` | SemVer 2.0 (`MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]`), at most 64 characters. Build metadata is accepted and dropped: the registry version is `MAJOR.MINOR.PATCH[-PRERELEASE]`, so `1.0.0+a` and `1.0.0+b` are the same version. |
| `title` | Non-empty, at most 80 characters. |
| `description` | Optional, at most 500 characters. |
| `data` | 1 to 64 entries. Each is an exact file path (`card.json`) or a directory prefix ending in `/` (`media/`). No globs: `*?[]{}` are rejected. Each entry follows the path contract below. |

Unknown fields are rejected. A new `format` value, not new optional fields,
signals an incompatible manifest.

A data entry must not cover a reserved name. An entry is rejected when its
first segment (the whole entry for a root file) is any of:

- `index.html` (the viewer entry)
- `_type` (viewer files)
- `_access.json`, `_spot.json`, or a name starting with `_screenshot.`
  (platform files)
- `spot.js`, `spot-live.js`, `spot.d.ts` (SDK files served before storage)
- `api` (site APIs)

Package files themselves are not restricted by these names: they are served
only under `/_type/`.

### Package validation

Upload is strict; nothing is dropped silently.

- Every path follows the path contract.
- `type.json` and `index.html` are present at the package root.
- Dot segments (`.git/…`, `.DS_Store`, `a/.env`), `node_modules` segments, and
  `Thumbs.db` are rejected, not dropped.
- At most 2000 files and 100 MiB per upload, the deploy limits.
- No symlinks: the CLI refuses a package directory that contains one.

### Identity and immutability

A version is identified by `name` and registry `version`. Its package hash is

```text
sha256:<hex of sha256 over the inventory lines>
```

where the inventory is every package file sorted by path (bytewise), one line
per file: `<path>\t<size in bytes>\t<hex sha256 of the bytes>\n`.

- Re-uploading the same name, version and hash is idempotent: `200`, nothing
  changes, the yank state is untouched, and follow resolution runs again.
- Uploading different bytes under an existing version is `409
  type_version_exists`, also when that version is yanked.
- Prereleases (`1.1.0-rc.1`) are ordinary, uploadable versions. They are
  never chosen as "latest" and never followed.
- Versions are never deleted. Yank removes a version from selection.

## Path contract

One validator covers package paths, typed deploy paths, data declarations,
and site-host request paths.

- Relative, `/`-separated, at most 512 characters.
- Rejected: a leading `/`, backslashes, empty segments (`a//b`), `.` and `..`
  segments, control characters (U+0000–U+001F, U+007F), `:`.
- Duplicate paths and file/directory collisions (`a` beside `a/b`) are
  rejected.
- Prefix matching is segment-bounded: `media/` covers `media/x.png`, not
  `mediax.png`; `card.json` covers only `card.json`.
- Request paths are percent-decoded once. On site hosts a request whose raw
  path contains an encoded separator (`%2F`, `%5C`, any case) or whose decoded
  path has a backslash, control character, `.`/`..` segment, or an empty
  segment other than a trailing slash is answered `400` before routing.

## Serving on a typed site's origin

Request order is unchanged: lifecycle, visitor access (`_access.json`),
embedded SDK files (`/spot.js`, `/spot-live.js`, `/spot.d.ts`), then:

| Path | Served from |
| --- | --- |
| `/`, `/index.html` | the pinned version's `index.html` |
| `/_type/<path>` | the pinned version's file `<path>`; `type.json` and `SKILL.md` are not served there; no directory index; `/_type` and `/_type/` are `404` |
| anything else | the site's own files, as for an untyped site (including `dir/` → `dir/index.html`) |

Only files in the pinned version's inventory are served under `/_type/`.

### Caching

- Authorization and lifecycle run before any conditional evaluation: a viewer
  whose access was revoked never gets a `304`. `HEAD` follows `GET`.
- Type files (`/`, `/index.html`, `/_type/…`):
  - `ETag: "<version>-<file sha256 hex>"` (strong), compared against
    `If-None-Match` before storage is opened. A match returns `304`. This
    format is part of the contract and stable: `<version>` is the pinned
    registry version and the hash is the file's 64 lowercase hex digits, so
    the version is everything between the opening quote and the last `-`
    (a prerelease version contains `-` itself). A proxy may weaken the tag to
    `W/"…"`; strip that prefix before parsing.
  - `Cache-Control: private, no-cache`, on public and restricted sites.
  - No `Last-Modified`.
- Typed site data (every other path): `Cache-Control: no-store`, no
  `Last-Modified`, no `ETag`.
- No `immutable` and no shared-cache freshness on site origins.

A page open across a repin keeps the viewer it loaded. A page load that
straddles a repin can get the old entry with new `/_type/` assets; a reload
fixes it (see viewer conventions).

## Deploys

`POST /api/deploy` (apex, multipart) keeps its fields (`site`, `files`,
`preserve_access`) and gains three optional text fields:

| Field | Value |
| --- | --- |
| `type` | `name@version`, the exact registry version the writer worked against. No ranges, no bare names, no build metadata. |
| `type_target` | `version` of the same type: repin in the same deploy; the deploy's data is validated against the target. Requires `type` and an existing typed site. |
| `type_track` | `pinned` or `major`. Requires `type`. |

A deploy without `type` is an ordinary deploy, unchanged, except that it is
refused on a typed site.

### Typed normalization

A deploy with `type` skips the ordinary normalization: no common-folder
stripping and no root `index.html` requirement. File paths follow the path
contract; paths with a dot or `node_modules` segment and `Thumbs.db` are
dropped, as in ordinary deploys. Then, against the version the data is
validated against (below):

- Refused: root `index.html` and anything under `_type/`.
- Allowed at the root: `_access.json`, `_spot.json`,
  `_screenshot.{jpg,jpeg,png,webp}`.
- Every other path must be covered by a `data` entry.
- Every `*.json` file under a data entry must parse as JSON.
- A deploy may contain only platform files, or no files at all: that is a
  valid empty instance.

The first offending path is reported as `400` with `code: "type_data_invalid"`
and `path`.

Sync semantics are unchanged: the uploaded set replaces the site's files and
stale files are removed, so an untyped site deployed with `type` loses its old
root `index.html`. `preserve_access` behaves as for any deploy.

### Pin rules

The site's current pin is read under the site's mutation lock, after deploy
authorization.

**New pin** (create; recreate of a deleted site; an untyped active site; a
provisioning site): `type` names an existing, non-yanked version; the data is
validated against it; the site pins it with track `type_track` (default
`pinned`). `type_target` is `400 invalid_type_field`.

**Typed site, deploy without `type`:** `409 type_required`.

**Typed site, different type name:** `409 type_name_mismatch`.

**Typed site pinned to `name@V`, deploy `type=name@v`:**

- `v == V`: accepted.
  - Without `type_target`: data validated against `V`; the pin stays `V`; the
    track becomes `type_track` when given.
  - With `type_target=T`: `T` must exist and not be yanked; data validated
    against `T`; the site pins `T` with track `type_track` (default
    `pinned`).
- `v != V`, the site's track is `major`, no `type_target`, `v` exists (yanked
  or not), and `v` has `V`'s major: accepted. Following moved the pin within
  an unchanged data contract. Data validated against `V`; the pin stays `V`;
  the track becomes `type_track` when given.
- Otherwise: `409 type_pin_mismatch` with `current`. The writer re-imports.

When the resulting track is `major`, follow resolution runs for the site
before the response, so a create with `type_track=major` lands on the newest
eligible version of its major.

### Authorization

The existing deploy roles apply (owner, admin, maintainer, publishing key).
A publishing-key deploy may create a typed site, with any `type_track`, but
may not send `type_target` or change an existing site's track: `403
type_repin_forbidden`. `_access.json` never grants repin authority.

### Ordering

A typed deploy holds the site mutation lock from authorization through the
pin check, validation, writes, and the pin commit. The pin and track are
committed in one short SQLite statement fenced by the deploy's content
generation, after the files are written and before the site is activated.

### Response

```json
{
  "site": "deck",
  "url": "https://deck.spot.example.com/",
  "files": 3,
  "version": "20261017T120000.000000000Z",
  "type": {"name": "card", "version": "1.0.0", "track": "pinned", "title": "Card"}
}
```

`type` is present only for typed sites.

## Repin and following

### `PUT /api/sites/{name}/type`

Apex only, same-origin, JSON body, at least one field:

```json
{"version": "1.2.0", "track": "major"}
```

- Callers: the site's owner, a platform admin, or a maintainer, resolved by
  identity (not by publishing key), the same gate as `PUT
  /api/sites/{name}/access`. Active sites only.
- `version` set: the target must be an existing, non-yanked version of the
  site's type. The site's retained data must pass the target's typed
  validation (`409 type_data_incompatible` with `path` otherwise). The track
  becomes `track` when given, else `pinned`, so a backward repin is not
  undone by following.
- Only `track` set: changes the track and keeps the version, also when the
  current version is yanked (freeze).
- When the resulting track is `major`, follow resolution runs.
- Untyped site: `409 site_untyped`.

Response `200`:

```json
{
  "site": "deck",
  "type": {"name": "card", "version": "1.2.0", "track": "major", "title": "Card"},
  "previous": {"name": "card", "version": "1.0.0", "track": "pinned", "title": "Card"}
}
```

### Tracks

- `pinned` (default): the site serves its exact version until a manager
  repins.
- `major`: the site follows the newest non-yanked, non-prerelease version
  within its current major, by SemVer precedence.

Follow resolution runs on every successful upload (including an identical
re-upload), on un-yank, on creation with `major`, on enabling `major`, and on
a repin that keeps `major`. Per site it takes the site's mutation lock, then
moves the pin in one SQLite statement conditioned on the site's current type,
version and track. Followers only move forward. A yank never moves sites; a
yanked version keeps serving the sites that pin it.

Minor and patch versions promise an unchanged data contract and a backward
compatible `SKILL.md`. A new major is a new contract, crossed only by an
explicit repin, normally a deploy with `type_target` and migrated data.

### Deploy events

A deploy, a repin that changes the version, and a follow move publish the
`_spot_deploys` realtime event for the site:

```json
{"type": "deploy", "collection": "_spot_deploys", "id": "<stamp>", "version": "<stamp>",
 "url": "https://deck.spot.example.com/", "site_type": "card@1.2.0"}
```

`site_type` is present for typed sites. Events are best effort: they are lost
on slow sockets and are not replayed after a reconnect. In delegated-login
deployments only signed-in visitors receive them; otherwise the deployment's
identity mode decides who may open `/api/ws`. Viewers must not depend on
events and must poll as a fallback.

Repins and follow moves are recorded in the site's deploy audit as action
`type`.

## Types API

All `/api/types` routes are apex only and same-origin, like `/api/sites/*`.
Admin routes require a platform admin (`SPOT_ADMIN_EMAILS` /
`SPOT_ADMIN_GROUPS`) resolved by identity; a request carrying a publishing key
(`Authorization` header) is refused with `403 admin_required`. Read routes
require an identified caller (`401` otherwise). Nothing type-related is served
on a site origin except the pinned viewer.

### `POST /api/types` (admin)

Multipart form, one `files` part per package file, the part's `filename`
carrying the package-relative path (the deploy upload shape):

```sh
curl -F 'files=@type.json;filename=type.json' \
     -F 'files=@index.html;filename=index.html' \
     -F 'files=@app.js;filename=app.js' \
     https://spot.example.com/api/types
```

Uploads of the same type name are serialized. Inside that lock Spot checks
the existing version and hash before writing any object, writes every file,
then records the version; only recorded inventory files are ever served.

Response `201` (new) or `200` (identical re-upload):

```json
{
  "created": true,
  "type": { "...": "version detail, below" },
  "followed": [{"site": "deck", "from": "1.0.0", "to": "1.1.0"}]
}
```

### `POST /api/types/{name}/{version}/yank` (admin)

Removes the version from selection: no new pins, repins, `type_target` or
follow moves to it. Sites pinning it keep serving it. Idempotent. Response
`200` with the version summary (`yanked: true`).

### `DELETE /api/types/{name}/{version}/yank` (admin)

Un-yanks. Idempotent. Runs follow resolution. Response `200` with the version
summary and `followed`.

### `GET /api/types`

Each type's newest non-yanked, non-prerelease version; types without one are
omitted.

```json
{"types": [{"name": "card", "version": "1.1.0", "title": "Card", "description": "…", "skill": true}]}
```

`GET /api/types?all=true` lists every type name with at least one version,
including types whose versions are all yanked or prerelease. Each entry has
the same shape plus `selectable`: `true` when the type has a non-yanked,
non-prerelease version, which is then the entry's version (as in the default
listing); `false` otherwise, and the entry describes the newest version by
SemVer precedence, yanked or not. Any identified caller may use it. Types are
ordered by name in both forms.

```json
{"types": [
  {"name": "card", "version": "1.1.0", "title": "Card", "description": "…", "skill": true, "selectable": true},
  {"name": "draft", "version": "0.1.0-rc.2", "title": "Draft", "description": "", "skill": false, "selectable": false}
]}
```

`all` accepts `true` or `false`; any other value is `400 invalid_type_field`.

### `GET /api/types/{name}`

```json
{
  "name": "card",
  "latest": "1.1.0",
  "versions": [
    {"version": "1.2.0-rc.1", "title": "Card", "prerelease": true, "yanked": false,
     "yanked_at": null, "uploaded_at": "2026-10-17T12:00:00Z", "skill": true}
  ]
}
```

Versions are ordered newest first by SemVer precedence. `latest` is `null`
when no version is selectable. Admins additionally see `uploaded_by` and
`pins` (active sites pinning the version) on each version. `404
type_not_found` when the type has no versions.

### `GET /api/types/{name}/{version}`

The version detail:

```json
{
  "name": "card",
  "version": "1.1.0",
  "title": "Card",
  "description": "…",
  "data": ["card.json", "media/"],
  "hash": "sha256:…",
  "skill": true,
  "prerelease": false,
  "yanked": false,
  "yanked_at": null,
  "uploaded_at": "2026-10-17T12:00:00Z",
  "manifest": {"format": 1, "...": "type.json as uploaded"},
  "files": [{"path": "index.html", "size": 1234, "sha256": "…"}]
}
```

`version` in the URL is the registry version (no build metadata).

### `GET /api/types/{name}/{version}/skill`

`SKILL.md` as `text/markdown; charset=utf-8`. `404 type_skill_not_found` when
the version has none.

### `GET /api/types/{name}/{version}/package`

`application/zip`, `Content-Disposition: attachment;
filename="<name>-<version>.zip"`. The ZIP mirrors what a site origin serves:
`index.html` at its root and every served file under `_type/` (not
`type.json` or `SKILL.md`). Yanked versions remain downloadable.

For a local preview, unpack it into an empty directory, copy the instance
data beside it, and serve the directory with any static server.

### Site listings and lookup

`GET /api/sites/mine`, `/manageable`, `/visible` (including the single-site
`names=` lookup) and `/public` entries carry, for typed sites only:

```json
"type": {"name": "card", "version": "1.1.0", "track": "pinned", "title": "Card"}
```

`title` is the pinned version's title.

### Source download

`GET /api/download` on a typed site's origin sets `X-Spot-Type:
name@version`. The ZIP contains the site's own files (never the viewer). A
typed site with no files returns an empty ZIP rather than `404`. The pin and
the file list are read under the site lock; the lock is released before the
ZIP streams. A repin during the stream can label migrated bytes with the old
version; the next publish with that `type` gets `409 type_pin_mismatch`.

### Cloudflare Pages

Typed sites are not eligible for Cloudflare Pages publishing: the status
eligibility lists the reason and publish returns `400`.

### Errors

Errors are JSON: `{"error": "<message>", "code": "<code>", ...}`. Codes:

| Status | Code | Meaning | Extra fields |
| --- | --- | --- | --- |
| 400 | `invalid_type_field` | malformed `type`, `type_target`, `type_track`, or repin body | |
| 400 | `invalid_package` | package fails validation | `path` when a path is at fault |
| 400 | `type_data_invalid` | deploy file outside data paths, reserved, or invalid JSON | `path` |
| 400 | `type_not_found` | deploy or repin names a version that does not exist | |
| 403 | `admin_required` | types admin route without platform-admin identity | |
| 403 | `type_repin_forbidden` | publishing key sends `type_target` or changes a track | |
| 404 | `type_not_found` | unknown type or version on a read route | |
| 404 | `type_skill_not_found` | version has no `SKILL.md` | |
| 409 | `type_version_exists` | different bytes under an existing version | `hash` (the recorded one) |
| 409 | `type_yanked` | new pin, `type_target` or repin to a yanked version | |
| 409 | `type_pin_mismatch` | `type` differs from the site's pin | `current` (`name@version`) |
| 409 | `type_name_mismatch` | `type` names another type | `current` |
| 409 | `type_required` | typed site deployed without `type` | `current` |
| 409 | `site_untyped` | repin of an untyped site | |
| 409 | `type_data_incompatible` | retained data fails the repin target | `path` |

Other errors keep their existing messages without a `code`.

## Viewer conventions

- The entry is `index.html`. Reference viewer assets with absolute `/_type/…`
  URLs. A relative `src="app.js"` resolves to site data, and `<base
  href="/_type/">` would also redirect data fetches.
- Read data root-relative: `fetch('/card.json')`.
- A viewer may read the pinned version from the entry's `ETag` (see Caching),
  for example to detect a repin while polling without deploy events:
  `fetch('/', {method: 'HEAD', cache: 'no-cache'})` and compare the version
  with its own; reload when they differ.
- Load the viewer's code at startup, ideally one bundle. Lazy-loaded viewer
  code fetched after a repin is the new version's.
- Tolerate missing data files (`404`): show a placeholder and keep going.
  Instances can be empty and are filled progressively.
- Refresh data on the `_spot_deploys` event when a websocket is available
  (`/api/ws`, send `{"type": "subscribe", "collection": "_spot_deploys"}`).
  If the event's `site_type` differs from the viewer's own version, reload the
  page. Do not depend on events: when the socket is unavailable (for example
  an anonymous visitor in a delegated-login deployment) or events are lost,
  poll the data index at a bounded interval, at most every few seconds and
  backing off, while placeholders remain.
- Do not depend on `/_type/type.json` or `/_type/SKILL.md`; they are not
  served.
- The viewer runs with the visitor's identity on the site origin and may use
  `/spot.js`. It is admin code: Spot runs nothing on the server for a type.

## Upgrade and rollback

The upgrade to the first release with site types is additive: existing sites
gain empty type columns and keep working unchanged.

- Back up the SQLite database and site storage together, as one consistent
  pair. Site storage includes the `_types/` root that holds every package's
  files; a database backup without it leaves recorded versions with no
  viewer bytes.
- A binary-only downgrade to 0.7.0 does not preserve typed sites. 0.7.0 opens
  the database and serves ordinary sites, but on a typed site `/` and
  `/_type/…` answer `404`, while the site's data paths stay reachable under
  its access policy.
- Avoid writes to typed sites while downgraded. A 0.7.0 deploy replaces the
  site's files but keeps the type columns it does not know about, so after
  re-upgrading the old viewer is served again over the replaced content (an
  `index.html` deployed meanwhile is hidden, and data the viewer expects may
  be gone).
- Otherwise, to roll back, restore a consistent pre-upgrade backup of the
  database and site storage.

## CLI

```sh
spot types list
spot types show <name>[@<version>]
spot types skill <name>@<version>
spot types upload <dir>                 # admin
spot types yank <name>@<version>        # admin
spot types unyank <name>@<version>      # admin
spot types pull <name>@<version> [dir]  # unpack the viewer package
spot types repin <site> [--version <v>] [--track pinned|major]
spot deploy --type <name>@<version> [--type-target <v>] [--follow major|--pinned] [name] [dir]
```

A typed `spot deploy` uploads the directory as-is: no `index.html` is
required. `spot types pull` needs `unzip`. The types commands use the
caller's ambient identity, never `SPOT_PUBLISH_KEY`.

Mutating routes (`POST /api/types`, yank, un-yank, repin) share the deploy
rate limit (a burst of 3, then one request every two seconds per client
address); a secret-proven forward-auth proxy is exempt, as for deploys.
