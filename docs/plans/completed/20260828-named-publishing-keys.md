# Named Publishing Keys

**Status:** Completed

## Implementation Checklist

- [x] Add the publishing-key schema, additive migration coverage, credential
  generation, hashed storage, lookup, listing, revocation, and usage tracking.
- [x] Add authenticated apex APIs and `/spots` management UI for creating,
  copying, listing, rotating, and revoking independently named keys.
- [x] Add publishing-key authentication to `POST /api/deploy` only, with a
  fixed site-prefix boundary and owner-only create/update/recreate semantics.
- [x] Extend deploy audits and authenticated site-management responses with
  trusted publisher attribution without exposing it in the public gallery.
- [x] Add `SPOT_PUBLISH_KEY` support to the CLI and regenerate every checked-in
  and embedded CLI copy.
- [x] Document GitHub Actions usage, credential rotation, network requirements,
  and the distinction between Spot deploy keys and Cloudflare publication.
- [x] Add unit, migration, handler, integration, CLI, security, and regression
  coverage and pass the repository validation commands.

## Overview

Spot normally authenticates deploys from a NetBird or Tailscale peer, a trusted
forward-auth proxy, or the fixed identity in single-user mode. Hosted CI runners
may not be members of that identity network. A repository also commonly needs
to create a different preview site for every pull request, so a credential
bound to one pre-existing site is too narrow.

Add named publishing keys intended for repository automation. Each key belongs
to the stable Spot identity that created it and is constrained to one fixed site
prefix:

```text
Name:         GitHub Actions · melonamin/spot
Site prefix:  spot-pr-
Owner:        sasha@example.com
```

That key may create, update, or owner-recreate `spot-pr-123`, but it may not
deploy `production`, mutate a matching site owned by someone else, delete a
site, manage Cloudflare publication, or authenticate any other Spot API. A user
may create multiple independently revocable keys, normally one per repository.

The publisher name is trusted credential metadata rather than caller-supplied
input. Deploy audits retain both the human owner and the named automation
credential, allowing `/spots` to say that a preview was last deployed by a
specific GitHub Actions workflow without exposing repository names publicly.

## Settled Design Decisions

- Use repository-oriented, fixed-prefix keys rather than strictly per-site
  keys. Pull-request sites do not exist when their workflow credential is
  provisioned.
- Permit multiple keys per owner and overlapping prefixes. Overlap is necessary
  for no-downtime rotation; the UI warns about it but does not reject it.
- Require a non-empty prefix that starts with a lowercase letter or digit, uses
  only lowercase letters, digits, and hyphens, ends in `-`, and leaves room for
  at least one suffix character within Spot's 63-character DNS-label limit.
- Do not reserve a prefix. Site names remain globally first-claim-wins, and a
  key cannot take over an existing site owned by another identity.
- A site first created by a key has the key creator as its immutable human
  owner. The credential name is attribution, never an ownership principal.
- Give keys owner-only deploy authority within their prefix. They do not inherit
  platform-admin or `_access.json` maintainer privileges from the creator's
  current groups.
- Permit create, update, and owner-authorized recreation. Do not permit delete,
  release, Cloudflare management, publishing-key management, or authentication
  to any non-deploy endpoint.
- Treat `_access.json`, `_spot.json`, screenshots, and all other accepted deploy
  files exactly like a normal owner deploy. Existing policy-transition and
  failure-recovery guarantees remain in force. Repository workflows using a
  key are trusted to control those files, including maintainers and capability
  policy.
- Use the standard `Authorization: Bearer <key>` request header. When a Bearer
  credential is present on `/api/deploy`, invalid credentials fail with `401`
  and never fall back to ambient mesh identity.
- Expose the key secret once. Store only a SHA-256 hash of a cryptographically
  random 256-bit secret and compare hashes in constant time.
- Rotate by creating a second key, updating the repository secret, observing
  the replacement's `last_used_at`, and revoking the old key. Do not regenerate
  a credential in place or add automatic expiration in the first version.
- Retain revoked key metadata for audit readability, clear its secret hash, and
  make revocation immediate and irreversible.
- Keep publisher attribution in SQLite deploy audits and authenticated
  management responses. Do not write it into deploy-controlled `_spot.json` or
  expose it through `/api/sites/public` and Gallery.
- The feature authenticates an HTTP deploy but does not provide network reach.
  A hosted runner still needs a routable HTTPS path to the Spot apex.

## Goals

- Let a GitHub Actions workflow create or update one preview site per pull
  request without joining the mesh.
- Constrain each repository secret to a recognizable site-name namespace.
- Preserve the human creator as the immutable owner of every site the key
  creates.
- Make repository credentials independently named, observable, rotatable, and
  revocable.
- Attribute successful and denied owner-level deploys to the authentication
  method and named credential without putting secrets in logs or metadata.
- Reuse the existing deploy validation, lifecycle, synchronization, policy
  transition, storage rollback, audit, and live-reload behavior.
- Keep ordinary mesh, forward-auth, dev, and single-user deploys compatible.

## Non-Goals

- GitHub OIDC or other workload-identity federation.
- Arbitrary glob or regular-expression site scopes.
- Prefix reservation or a general project/organization namespace system.
- Per-key permissions beyond the fixed prefix and deploy-only capability.
- Site deletion or automatic pull-request cleanup.
- Cloudflare Pages publication through a publishing key.
- Making a key a visitor identity for site access, realtime rooms, database,
  files, AI, Slack, or any other SDK feature.
- Recovering, displaying, or exporting a secret after its creation response.
- Automatic expiration or scheduled rotation.
- Transferring site ownership when a key is rotated or revoked.

## User Experience

Add an **Automation keys** section to `/spots`, above or below the manageable
site list. It is account-scoped because one key can create many sites and may
exist before any matching site does.

The creation dialog accepts:

- **Name**: a trimmed, human-readable label such as
  `GitHub Actions · melonamin/spot`, limited to 80 characters.
- **Site prefix**: a literal prefix such as `spot-pr-`, validated without
  silently lowercasing or otherwise rewriting it.

On success, show the full secret exactly once with a copy button and a compact
GitHub Actions example. Closing the result dialog discards the secret from
browser state. A later list response never includes it.

Each list row shows the name, `prefix*`, creation time, last successful use,
owner for an administrator view, status, and a revoke action. The UI warns when
an active key owned by the same user has an overlapping prefix, but creation
continues so rotation remains possible. Revocation requires an explicit
confirmation and updates the row in place.

An authenticated site card may show:

```text
Deployed 12 minutes ago by GitHub Actions · melonamin/spot
```

Mesh and forward-auth deploys retain their existing human attribution. Public
Gallery cards do not receive publisher name, key ID, repository label, or
authentication-method fields.

## Credential Format and Storage

Use a self-identifying token with a versioned marker, public lookup ID, and
random secret:

```text
spot_pk_<public-id>_<base64url-secret>
```

Generate both components with `crypto/rand`. The public ID should contain
enough entropy to avoid enumeration and remain short enough for logs and UI;
the secret must contain at least 32 random bytes and use unpadded base64url.
Encode the public ID as fixed-length lowercase hex, then parse by removing the
known `spot_pk_` prefix and cutting once at the delimiter after that fixed-length
ID. Treat the entire remainder as the unpadded base64url secret because `_` is a
valid base64url character. Reject an invalid prefix, ID, encoding, empty secret,
or unexpected component length before querying SQLite.

Add a table along these lines to `server/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS publishing_keys (
    id text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    name text NOT NULL,
    site_prefix text NOT NULL,
    secret_hash blob,
    created_at datetime NOT NULL DEFAULT (...),
    last_used_at datetime,
    revoked_at datetime
);
```

Require exactly one stable owner key: lowercased email when available,
otherwise peer IP. Do not persist creator groups into the credential. Add an
owner lookup index suitable for listing active and revoked keys. The table is
not attached to a site foreign key because it intentionally predates and spans
matching sites.

Store `sha256(secret)` rather than the token or full token hash. The random
secret's entropy makes a fast hash appropriate, and lookup by the independently
random public ID prevents a table scan. Compare the stored and presented hash
with `crypto/subtle.ConstantTimeCompare`. Clear `secret_hash` and set
`revoked_at` atomically when revoking. Set `last_used_at` only after a
successful deploy; a telemetry update failure must be logged but must not turn
an already committed deploy into a client-visible failure.

Because the new table is created by the embedded schema on every startup,
existing databases gain it through `CREATE TABLE IF NOT EXISTS`. Audit columns
added below still need idempotent `ensureColumn` migrations.

## Key Management API

Add apex-only, same-origin, rate-limited endpoints:

```text
GET    /api/publishing-keys
POST   /api/publishing-keys
DELETE /api/publishing-keys/{id}
```

All three use the normal mesh, forward-auth, or single-user identity path and
require a stable actor key. A publishing Bearer token never authenticates these
routes.

`POST /api/publishing-keys` accepts one strictly decoded JSON object:

```json
{"name":"GitHub Actions · melonamin/spot","site_prefix":"spot-pr-"}
```

Bound the request body, reject unknown fields and trailing JSON, validate both
values, generate the token, insert its hash and owner snapshot, and return
`201 Created` with the public record plus a top-level `secret`. If response
delivery is interrupted, the secret is unrecoverable; the user creates a new
key and revokes the uncertain one. Set `Cache-Control: no-store` on this
response, and request it with `cache: "no-store"` from `/spots`.

`GET` returns only the caller's active and revoked records newest-first without
hash material. There is no administrator-wide listing endpoint or UI.

`DELETE` authorizes the immutable key owner or a current platform admin,
revokes atomically, and returns success when the target is already revoked so
retrying is safe. Return `404` rather than revealing a key the actor may not
inspect. There is no un-revoke operation.

Keep the store and handler surfaces small and explicit, for example a
`PublishingKeyStore` responsible for persistence and a credential service
responsible for generation, parsing, hashing, and authentication.

## Deploy Authentication and Authorization

Publishing-key authentication belongs only in `handleDeploy`; do not add it to
the shared `resolveIdentity` path. Introduce a deploy-specific principal that
keeps the human actor separate from credential metadata:

```text
DeployPrincipal
  Actor                 stored key owner or resolved ambient identity
  AuthMethod            publishing_key, mesh, forward_auth, single_user, dev
  PublishingKeyID       public ID when applicable
  PublisherName         trusted key name when applicable
  RequiredSitePrefix    fixed prefix when applicable
  OwnerOnly             true for publishing keys
```

Resolve this principal before reading the multipart body. If an Authorization
header is present, require one well-formed Bearer value and authenticate it as
a publishing key. Invalid, revoked, malformed, or unknown keys return the same
generic `401 Unauthorized`. Never fall through to a mesh identity when a
Bearer value was supplied. Missing Authorization preserves current behavior.

After parsing and normalizing the site name, a key deploy requires a strict
`strings.HasPrefix(site, key.SitePrefix)` match, a non-empty suffix, and the
normal DNS-label validation. Reject mismatch with `403 Forbidden` before the
site mutation lock or any storage operation.

Do not authorize a key by merely reconstructing the creator's `Identity` and
calling the current role-bearing path. That could accidentally grant current
admin or maintainer authority. Add an owner-only registry authorization path,
sharing the existing transaction and generation logic, that permits:

- first claim of a missing matching name, owned by the stored key owner;
- update of an active matching site only when `SiteRecord.OwnedBy(actor)`;
- recovery of provisioning state and recreation of an owner tombstone under
  the same owner checks;
- no authorization through platform-admin configuration, groups, or stored
  `_access.json` maintainers.

The check must remain atomic with the existing claim/touch transaction so a
concurrent first deploy cannot turn a preflight ownership result into a
takeover. The handler must continue using the per-site mutation lock, content
generation, policy-transition reconciliation, rollback, storage synchronization,
metadata update, audit, completion, and realtime publication paths unchanged.

Keys may upload `_access.json`. This lets a repository define access and
maintainers for its own preview namespace, while the existing rule that an
incoming policy cannot authorize its own deploy remains intact.

## Audit and Attribution

Add additive, non-secret columns to `site_deploy_audit`:

```text
auth_method       text NOT NULL DEFAULT ''
publisher_key_id  text NOT NULL DEFAULT ''
publisher_name    text NOT NULL DEFAULT ''
```

Populate them from the deploy principal for success, failure, and authorized
denial events. Preserve the existing actor email, peer IP, name, groups,
authorization role, content hash, and lifecycle fields. A publishing-key
deploy should record the stored human owner in `actor_*`, `owner` as its
authorization role, and the key label in `publisher_name`.

Do not create site audit rows for invalid credentials or prefix mismatches
against names outside the key's scope. Log a bounded security event using at
most the parsed public key ID and requested site, never the token, secret,
hash, or Authorization header. Existing rate limits remain in force.

Extend the latest-success subqueries used by manageable site listings so the
authenticated `/api/sites/manageable` response can include a small
`last_deploy` object with time, authentication method, and publisher name. Keep
the compatibility `/api/sites/mine` response coherent if `/spots` still
consumes it, but explicitly omit these fields from public-site queries and
Gallery JSON.

Snapshot publisher name and ID into every audit row. Historical attribution
must not depend on joining the mutable publishing-key record, so revocation,
renaming policy changes, or eventual key-record retention changes cannot erase
what performed a deploy.

## CLI Behavior

Teach the source CLI at `cli/spot` to read `SPOT_PUBLISH_KEY` from the process
environment only. Do not add it to the permissive config-file parser: the
checked-in parser intentionally accepts only `SPOT_URL`, and repository secrets
should not be silently persisted in `~/.config/spot/env`.

When the variable is non-empty, `deploy_upload` adds:

```text
Authorization: Bearer $SPOT_PUBLISH_KEY
```

to both the initial deploy and optional screenshot redeploy. `spot show deploy`
and `spot show watch` already funnel through the same upload function and gain
support automatically. Do not place the header in curl's process argument
vector. Pass a narrowly constructed curl config on standard input (the
generated token alphabet needs no config escaping), while keeping multipart
file data on ordinary file paths. Never evaluate the secret or print the config
or command.

On `401`, keep the existing friendly HTTP failure format while adding a concise
hint that `SPOT_PUBLISH_KEY` may be invalid or revoked. Never echo the value.

Update CLI usage and agent instructions to explain the variable, then run
`just generate` so `sdk/spot` and `server/static_assets/sdk/spot` exactly match
the source copy.

Document a GitHub Actions example:

```yaml
- name: Deploy Spot preview
  env:
    SPOT_URL: ${{ vars.SPOT_URL }}
    SPOT_PUBLISH_KEY: ${{ secrets.SPOT_PUBLISH_KEY }}
  run: ./spot deploy "spot-pr-${{ github.event.number }}" dist/
```

Make clear that the key authenticates Spot but does not create firewall, DNS,
TLS, VPN, or reverse-proxy reachability from a hosted runner.

## Error Handling and Security Properties

- Key creation uses `crypto/rand`; entropy-source failure returns `500` and
  writes no partial record.
- Creation validates before generation and inserts the hash before returning
  the one-time token.
- Unknown, malformed, hash-mismatched, revoked, or hash-cleared tokens share
  one `401` response and do not reveal which condition occurred.
- A supplied Bearer header is authoritative for `/api/deploy`; failure cannot
  downgrade to mesh or forward-auth authentication.
- Prefix checks operate on the final normalized site string and require a
  non-empty suffix, preventing `foo-` from authorizing a site named only
  `foo-` or an invalid DNS label.
- Owner-only authorization is enforced inside the registry transaction, not
  through a stale handler precheck.
- A key cannot act as a platform admin or maintainer, even if its creator
  currently has those roles for a foreign-owned site.
- Revocation clears the stored verifier in the same transaction that records
  `revoked_at`. Define the linearization point as the SQLite transaction that
  revalidates the active key and reserves the site's content generation. A
  revocation committed first rejects the deploy; an authorization reservation
  committed first may finish, while every later deploy is rejected. This is the
  precise meaning of immediate revocation for an in-flight upload.
- Key values, Authorization headers, and hashes never appear in logs, errors,
  audit rows, JSON list responses, HTML, or test failure output.
- Cross-origin browser requests remain subject to the existing apex and
  same-origin protections. Headerless CLI/API requests have no Origin and
  continue to work as they do today.
- Key management uses the existing deploy/database rate limits plus strict
  request body and field length bounds.
- Revoking a key never deletes, transfers, or otherwise changes sites it
  previously created.

## Component and File Boundaries

Expected primary touchpoints:

- `server/schema.sql`, `server/db.go`: tables, indexes, and audit migrations.
- New `server/publishing_keys.go`: credential format, store, validation, and
  management handlers, kept separate from mesh identity resolution.
- `server/handlers.go`: route registration, deploy-principal resolution seam,
  and server dependency wiring.
- `server/deploy.go`: deploy-specific authentication, prefix enforcement, and
  audit attribution propagation.
- `server/site_registry.go`: atomic owner-only deploy authorization and latest
  deploy attribution queries.
- `server/sites.go`: authenticated management response fields.
- `server/main.go`: construct and inject the publishing-key store; no global
  publishing-key environment configuration is required.
- `sdk/spots.html`: Automation keys UI and last-publisher display.
- `cli/spot`: `SPOT_PUBLISH_KEY` upload support and generated help text.
- `README.md`, `.env.example` only if useful, and `docs/ARCHITECTURE.md`:
  operator and security documentation. Do not add a server-side master key to
  `.env.example`.
- Focused `*_test.go` files beside each component plus integration tests where
  real SQLite and multipart behavior matter.

Prefer narrow interfaces such as `PublishingKeyAuthenticator` and
`PublishingKeyAdmin` over adding credential concerns to the broad identity
resolver. Keep secret handling out of `Identity`; an identity describes the
human owner, while a deploy principal describes how this request authenticated.

## Implementation Tasks

### Task 1: Persistence and Credential Service

- [x] Define the publishing-key record and public JSON model without secret or
  hash fields.
- [x] Add `publishing_keys`, its owner index, and idempotent startup behavior.
- [x] Implement strict name and fixed-prefix validation shared by API and store
  tests.
- [x] Implement random token generation, strict parsing, SHA-256 hashing,
  constant-time verification, and generic authentication errors.
- [x] Implement create, owner-only listing, authenticate, successful-use touch,
  and irreversible revoke operations.
- [x] Cover entropy failure through an injected random reader rather than
  weakening production generation.
- [x] Test fresh schema, restart idempotence, nullable timestamp scanning,
  revocation hash clearing, and overlapping prefixes.

### Task 2: Key Management API

- [x] Register apex-only management routes with same-origin and rate-limit
  wrappers consistent with other site administration APIs.
- [x] Implement strict bounded JSON creation and one-time secret response.
- [x] Implement owner-only key listing without exposing verifier material.
- [x] Implement owner/admin idempotent revocation with unauthorized `404`
  behavior.
- [x] Test missing identity, unstable identity, forward-auth, single-user,
  ordinary owner, foreign user, admin, malformed payload, duplicate/unknown
  fields, and response redaction.

### Task 3: Deploy Principal and Owner-Only Authorization

- [x] Add a deploy-specific principal and authentication method constants.
- [x] Resolve and verify Bearer credentials before multipart processing while
  preserving the current ambient identity path when the header is absent.
- [x] Reject malformed/multiple/non-Bearer Authorization values generically and
  prove there is no ambient-identity fallback.
- [x] Enforce the literal prefix and non-empty suffix before site locking.
- [x] Add an atomic owner-only registry path that shares claim, generation,
  lifecycle, cancel, and completion behavior with normal deploy authorization.
- [x] Revalidate the credential and reserve the site's content generation in
  one SQLite transaction, establishing the documented revocation ordering.
- [x] Preserve all existing metadata, access policy, rollback, and realtime
  behavior for key-authenticated deploys.
- [x] Test missing-site creation, same-owner update, tombstone recreation,
  provisioning recovery, prefix mismatch, boundary-like names, foreign owner,
  admin-only access, maintainer-only access, races, and revoked in-flight keys.

### Task 4: Audit and Management Attribution

- [x] Add audit columns to the fresh schema and idempotent additive migrations
  for upgraded databases.
- [x] Carry authentication method, public key ID, and publisher snapshot through
  every deploy audit helper and failure path.
- [x] Keep invalid or out-of-prefix attempts from polluting unrelated site
  audit history.
- [x] Extend authenticated latest-deploy queries and JSON without altering
  public Gallery responses.
- [x] Show trusted latest-publisher text on `/spots` with safe text rendering.
- [x] Test migration restart safety, revoked-key historical display, public
  response omission, normal mesh attribution, and publishing-key attribution.

### Task 5: Automation Keys UI

- [x] Add a responsive Automation keys section consistent with `/spots`'s
  deliberate fixed-light visual language.
- [x] Add create, one-time secret, copy, overlap warning, revoke confirmation,
  empty, loading, and error states.
- [x] Ensure the secret leaves live DOM/browser state when its dialog closes and
  never enters `localStorage`, URLs, analytics, or subsequent list results.
- [x] Show timestamps and revoked state clearly without an administrator-wide
  listing mode.
- [x] Add static UI contract tests and manually verify dialog interaction,
  one-time-secret cleanup, copy feedback, and desktop and narrow layouts.

### Task 6: CLI and Generated Assets

- [x] Read `SPOT_PUBLISH_KEY` only from the environment and pass a Bearer header
  to every deploy upload through curl configuration on stdin, not process argv.
- [x] Keep the key out of debug/error output and add a useful `401` hint.
- [x] Add shell-level CLI tests with a local HTTP fixture that captures headers
  and verifies the secret is not printed.
- [x] Cover ordinary deploy, screenshot redeploy, Show deploy, and absence of
  the environment variable.
- [x] Run `just generate` and verify source, served, and embedded CLI copies are
  synchronized.

### Task 7: Documentation and End-to-End Validation

- [x] Update README deployment/authentication documentation and remove the
  statement that Spot has no per-site deploy keys in favor of precise
  namespace-key language.
- [x] Update architecture documentation with the credential boundary, request
  flow, owner-only authorization, audit model, revocation window, and public
  metadata exclusions.
- [x] Document GitHub repository-secret setup, pull-request naming, rotation,
  revocation, and the requirement for routable HTTPS access to the apex.
- [x] Clearly distinguish internal Spot deploy authentication from optional
  Cloudflare Pages internet publishing and its server-side API token.
- [x] Add an integration flow that creates a key through ambient identity,
  deploys two matching PR sites, rejects an outside-prefix site, rejects a
  foreign-owned collision, revokes the key, and rejects its next deploy.
- [x] Run formatting, generation, build, unit, integration, and E2E validation.

## Validation Commands

Run focused tests while implementing, then finish with:

```sh
just generate
cmp cli/spot sdk/spot
cmp cli/spot server/static_assets/sdk/spot
just build
just test
just test-integration
just e2e
```

Also inspect the final diff for accidental credential fixtures and verify no
test token resembles a usable production secret. For visible `/spots` changes,
capture desktop and narrow screenshots for the pull request.

## Completion Criteria

- A user can create multiple named keys with independent fixed prefixes and
  copy each secret once.
- A GitHub-hosted workflow with network access to Spot can use
  `SPOT_PUBLISH_KEY` to create and update pull-request preview sites.
- Every key-created site is immutably owned by the human creator, and a key
  cannot act through admin or maintainer authority.
- Prefix mismatch, foreign ownership, invalid credentials, and revocation fail
  closed before content mutation.
- Keys cannot authenticate deletion, Cloudflare, key management, or any other
  Spot API.
- Rotation with overlapping keys works without downtime, and historical audit
  attribution survives revocation.
- `/spots` shows trusted latest-publisher metadata while Gallery reveals none.
- Existing mesh, forward-auth, dev, and single-user deployments behave as
  before.
- Documentation and generated assets are synchronized, required tests pass,
  and visible UI states have been manually verified.
