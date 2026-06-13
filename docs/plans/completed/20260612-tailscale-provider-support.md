# Tailscale Provider Support

## Overview

Spot authenticates visitors by mapping WireGuard mesh peer IPs to users.
Today the only production resolver is NetBird (`NetbirdResolver` polls the
NetBird management API). This plan adds a `TailscaleResolver` that does the
same job against the Tailscale HTTP API, and lets deployments choose a
provider by setting that provider's env vars (auto-detect; configuring both
is a startup error).

Key decisions (agreed 2026-06-12):

- **Identity mechanism**: Tailscale HTTP API (`api.tailscale.com`) — poll
  devices + users, cache 30s. Mirrors `NetbirdResolver` exactly. Local
  `tailscale whois` / LocalAPI is explicitly out of scope (documented
  follow-up).
- **Groups**: parse the tailnet policy file (`GET /api/v2/tailnet/{t}/acl`
  with `Accept: application/json`), expose `group:team-payments` as both
  `team-payments` (stripped — the portable spelling, emitted by the access
  picker) and `group:team-payments` (so entries hand-copied from a tailnet
  policy still match). `_access.json` policies stay portable across
  providers.
- **Provider selection**: auto-detect from env. `NETBIRD_*` set → NetBird;
  `TAILSCALE_*` set → Tailscale; both set → fail fast with a clear error.
- **Testing approach**: regular (code first, tests in the same task).

What does NOT change (verified provider-agnostic): peer-IP extraction and
trusted-proxy logic (`identity.go:258-266`), Caddy `X-Forwarded-For`
handling, DB schema (`owner_peer_ip`), rate limiting, site ownership, the
`_access.json` policy format. Both providers use CGNAT 100.64.0.0/10 with
stable per-device IPs, so the peer-IP identity model carries over intact.

## Context (from discovery)

- **The one real NetBird coupling**: `server/identity.go:46-230` —
  `NetbirdResolver` + `netbirdPeer`/`netbirdUser`/`netbirdGroupRef` API
  structs. Everything else consumes the `IdentityResolver` /
  `DirectoryResolver` interfaces (`identity.go:25-44`), which already
  abstract the provider.
- **Wiring**: `server/main.go:180-189` picks the resolver;
  `main.go:92-110` (`validateDeploymentSafety`) *requires* NetBird vars for
  any shared (non-`.localhost`) deployment — must become provider-aware.
- **Hardcoded overlay name**: `scripts/deploy-prod.sh:7` defaults to
  `docker-compose.netbird.yml`; README deployment section documents it.
  The overlay itself contains zero NetBird-specific config (it exists to
  preserve source IPs via host networking) — only its name and comments are
  provider-specific.
- **User-facing strings/docs mentioning NetBird**: `server/handlers.go`
  (163, 206, 226, 361), `server/policy.go:22-23`, `README.md`, `.env.example`,
  `cli/spot:300,377` (+ `sdk/spot`, which mirrors it — check symlink vs copy),
  `sdk/spot.js:61`, `sdk/index.html:1071,1088`, `examples/demo/index.html:58`,
  `scripts/e2e.sh:25-26,272`.
- **Existing test patterns**: `server/identity_test.go` (`newNetbirdAPI`
  httptest stub), `server/policy_test.go:129-137` (full-server test with the
  stub resolver), `server/main_test.go` (config + deployment-safety tests).
- Tooling: `just test` (vet + unit), `just test-integration` (Postgres),
  `just e2e` (full stack).

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes
  in that task — success and error scenarios, table-driven where natural
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change (`just test`)
- maintain backward compatibility: existing NetBird deployments
  (e.g. spot.t1a.dev) must keep working with unchanged `.env` apart from the
  compose-file rename noted in Task 5 / Post-Completion

## Testing Strategy

- **unit tests**: required for every task; Tailscale API stubbed with
  `httptest` mirroring the `newNetbirdAPI` pattern
- **integration tests**: existing `-tags integration` suite (Postgres-backed,
  identity-independent) must stay green; the resolver↔server integration is
  covered by a `policy_test.go`-style full-server test against the Tailscale
  stub (Task 2)
- **e2e tests**: `just e2e` runs the full stack with the dev static identity;
  it must stay green after the overlay rename and env changes (Task 5).
  Real-tailnet verification is manual (see Post-Completion)

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): real-tailnet verification, production
  redeploy-procedure update, external follow-ups

## Implementation Steps

### Task 1: TailscaleResolver — devices and users to identity

**Files:**
- Create: `server/identity_tailscale.go`
- Create: `server/identity_tailscale_test.go`

`TailscaleResolver` mirrors `NetbirdResolver`'s shape exactly (mutex,
`byIP` map, `directory`, `fetchedAt`, `ensureFresh`, 30s TTL) — same
concurrency and caching semantics, different API translation.

Tailscale API specifics (vs NetBird):
- Auth header: `Authorization: Bearer <token>` (NetBird uses `Token <token>`)
- `GET /api/v2/tailnet/{tailnet}/devices` → `{"devices":[{"addresses":
  ["100.x.y.z","fd7a:..."], "user":"alex@t1a.com", "hostname":"mbp",
  "name":"mbp.tail1234.ts.net", "tags":["tag:ci"], ...}]}` — note the
  wrapper object and that `addresses` is a list; map **every** address to
  the identity, but `PeerIP` in every entry holds the device's canonical
  address (the IPv4 `100.64.0.0/10` one, falling back to `addresses[0]`).
  Site ownership falls back to `owner_peer_ip` (`site_registry.go:272`), so
  a device must present the same `PeerIP` whether it connects over v4 or v6.
- **Tagged devices are NOT user devices**: a tagged device's `user` field
  still carries the login of whoever tagged it, which would join against
  `/users` and hand a shared CI box a real employee's identity (email is
  the strong key for ownership and `_access.json`). When `tags` is
  non-empty, drop `Email`/`Name` — peer-only identity.
- `GET /api/v2/tailnet/{tailnet}/users` → `{"users":[{"loginName":
  "alex@t1a.com", "displayName":"Alex", ...}]}` — join on
  `device.user == user.loginName`; `loginName` becomes `Identity.Email`
- `PeerName` from `hostname` (short name, matching NetBird's peer name role)
- Tailnet defaults to `-` (the token's own tailnet)
- A device whose `user` has no matching users entry resolves with
  `PeerName`/`PeerIP` only — same degradation as NetBird's unmatched
  `user_id`

- [x] create `TailscaleResolver` struct + `NewTailscaleResolver(apiURL,
      token, tailnet string, ttl time.Duration)` in
      `server/identity_tailscale.go`; an empty `apiURL` defaults to
      `https://api.tailscale.com` here, NOT in `loadConfig` (a config-level
      default would make `TailscaleAPIURL` always non-empty and poison
      provider detection in Task 3)
- [x] implement `Resolve` and cached `fetch` for devices + users (Bearer
      auth, wrapper-object decoding, all addresses mapped to the canonical
      `PeerIP`)
- [x] implement `get` helper with wrapped errors
      (`fmt.Errorf("tailscale request %s: %w", ...)`) matching the NetBird
      error style
- [x] write `newTailscaleAPI` httptest stub + tests: resolve by IPv4 and
      by IPv6 (both return the canonical IPv4 `PeerIP`), unknown IP, user
      join (email/name), tagged device with resolvable `user` → peer-only
      identity, unmatched `user` → peer-only identity, cache honored within
      TTL (request counter, like `newNetbirdAPI`)
- [x] write error-case tests: non-200 status, malformed JSON
- [x] run `just test` — must pass before task 2

### Task 2: Tailnet ACL groups → Identity.Groups and directory

**Files:**
- Modify: `server/identity_tailscale.go`
- Modify: `server/identity_tailscale_test.go`
- Modify: `server/identity.go` (extract shared directory-sorting helper)
- Modify: `server/policy_test.go` (full-server test with Tailscale stub)

Tailscale has no groups API; groups live in the tailnet policy file:
`GET /api/v2/tailnet/{tailnet}/acl` with `Accept: application/json` returns
`{"groups": {"group:team-payments": ["alex@t1a.com"]}}`. Tailscale does
NOT allow groups inside groups (verify against current docs during
implementation), but member lists can contain non-email entries
(`autogroup:member`, bare domains), so membership needs a defensive
normalization pass rather than nested-group flattening. Group names are
exposed in **both** forms — stripped (`team-payments`) and prefixed
(`group:team-payments`) — so portable `_access.json` files use the
stripped spelling while entries hand-copied from a tailnet policy still
match (`policy.go` matching is already case-insensitive).

- [x] fetch + decode the policy file alongside devices/users
- [x] implement `normalizeGroups(raw map[string][]string)
      map[string][]string` — keep email members, decide what to do with
      non-email members (`autogroup:*`, domains, unexpected `group:` refs):
      skip silently vs log once vs error
      *(Sasha contribution point: ~8 lines, see Technical Details — the
      unknown-member strategy is a real design choice)*
- [x] populate `Identity.Groups` (sorted, deduped) from normalized
      membership by `loginName`, including both stripped and prefixed names
- [x] extract the sort block from `buildDirectory`
      (`identity.go:201-206`) into a shared `sortDirectory` helper; build
      the Tailscale directory (users by email, groups by stripped name —
      the picker emits the portable spelling only) with it
- [x] write tests: groups in resolved identity, both `team-payments` and
      `group:team-payments` spellings match, non-email members ignored per
      the chosen strategy, directory ordering (users before groups,
      case-insensitive)
- [x] add a `policy_test.go`-style full-server test: site with
      `_access.json` group allow, visitor resolved via the Tailscale stub →
      200; non-member → 403 (mirrors `policy_test.go:129-150`)
- [x] run `just test` — must pass before task 3

### Task 3: Provider auto-detection and wiring

**Files:**
- Modify: `server/main.go`
- Modify: `server/main_test.go`
- Modify: `server/handlers.go` (provider-neutral runtime messages)

Provider-detection fields must be credential-bearing only: NetBird
configured = `NetbirdAPIURL != "" || NetbirdAPIToken != ""` (matches
today's `main.go:93`); Tailscale configured = `TailscaleAPIToken != ""`
(plus the OAuth pair after Task 4). `TailscaleAPIURL` and
`TailscaleTailnet` are tuning knobs and must NEVER count as "configured" —
`loadConfig` leaves them empty-able (no `envOr` default for the URL; see
Task 1), otherwise every `.localhost` dev deployment turns "shared".

- [x] add config fields: `TailscaleAPIURL` (no default — defaulted inside
      `NewTailscaleResolver`), `TailscaleAPIToken`, `TailscaleTailnet`
      (default `-`); read `TAILSCALE_API_URL`, `TAILSCALE_API_TOKEN`,
      `TAILSCALE_TAILNET` in `loadConfig`
- [x] add the "exactly one provider" rule to `validateDeploymentSafety`
      (NOT inline in `loadConfig` — the struct-literal test pattern in
      `main_test.go:8-79` exercises `validateDeploymentSafety(cfg)`, which
      `loadConfig` already calls at `main.go:86`): any NetBird field set
      together with any Tailscale credential → "configure exactly one mesh
      identity provider"
- [x] extract the resolver selection from `main()` (`main.go:180-189`) into
      `newResolver(cfg config) (IdentityResolver, string)` returning the
      resolver and a log description, so selection is unit-testable
- [x] update `validateDeploymentSafety` (`main.go:92-110`): `shared` is
      true when a provider credential is set OR the domain is non-local;
      shared deployments require **a** provider (NetBird pair or
      `TAILSCALE_API_TOKEN`), with an error naming both options
      (`main_test.go:76` asserts the error contains "NETBIRD" — update the
      assertion alongside)
- [x] make runtime messages provider-neutral: `handlers.go:163` (503 hint
      names both env-var sets — MUST keep the literal prefix
      `identity resolver not configured`, `scripts/e2e.sh:266`
      case-matches it and e2e doesn't run until Task 5), `handlers.go:206`
      ("could not verify identity with the mesh provider"),
      `handlers.go:226`, `handlers.go:361` comment; `main.go:1-2` package
      comment
- [x] write table-driven tests in `main_test.go`: netbird-only →
      `*NetbirdResolver`, tailscale-only → `*TailscaleResolver`, both →
      config error, neither + dev identity → `*StaticResolver`, neither →
      nil; `validateDeploymentSafety` accepts tailscale-only shared config
      and still rejects provider-less shared config
- [x] run `just test` — must pass before task 4

### Task 4: OAuth client-credentials token source

**Files:**
- Modify: `server/identity_tailscale.go`
- Modify: `server/identity_tailscale_test.go`
- Modify: `server/main.go`, `server/main_test.go` (two new config fields)

Tailscale API access tokens expire (manual keys ≤90 days; OAuth-minted
tokens after 1 hour), which is unacceptable on the identity critical path
of an unattended server. Support an OAuth client (the Tailscale-recommended
service credential): when `TAILSCALE_OAUTH_CLIENT_ID` +
`TAILSCALE_OAUTH_CLIENT_SECRET` are set, exchange them at
`POST {apiURL}/api/v2/oauth/token` (client-credentials grant with
HTTP Basic client auth) and refresh the cached token shortly before expiry.
Static `TAILSCALE_API_TOKEN`
remains supported for quick setups; configuring both is a config error.
Hand-roll the exchange (~40 lines: Basic-auth form POST, decode `access_token` +
`expires_in`, refresh 60s early) — the resolver's fetch path is already
mutex-serialized, so no new locking is needed and no new dependency is
justified.

- [x] add a token-source seam to `TailscaleResolver` (static token or
      OAuth exchange with cached expiry)
- [x] config: `TAILSCALE_OAUTH_CLIENT_ID` / `TAILSCALE_OAUTH_CLIENT_SECRET`;
      token XOR oauth-pair validation; auto-detect treats the oauth pair as
      "Tailscale configured"
- [x] write tests: stub token endpoint (mint, expire, re-mint), 401 from
      token endpoint surfaces a clear error, static-token path unchanged
- [x] update `main_test.go` selection/validation tables for the new fields
- [x] run `just test` — must pass before task 5

### Task 5: Deployment artifacts — overlay rename, env, compose, scripts

**Files:**
- Rename: `docker-compose.netbird.yml` → `docker-compose.mesh.yml`
- Modify: `docker-compose.yml` (pass `TAILSCALE_*` env through to spot-api)
- Modify: `.env.example`
- Modify: `scripts/deploy-prod.sh` (overlay default at line 7)
- Modify: `scripts/e2e.sh` (clear `TAILSCALE_*` alongside `NETBIRD_*`)

The overlay is provider-agnostic (host networking to preserve mesh source
IPs) — it only *says* NetBird. Rename it once rather than duplicating it
per provider. ⚠️ Breaking for existing deployments' muscle memory: the
spot.t1a.dev redeploy procedure references the old name (Post-Completion).

- [x] `git mv docker-compose.netbird.yml docker-compose.mesh.yml`; update
      its comments to provider-neutral wording ("the caller's real mesh
      IP", usage line naming either provider's env vars)
- [x] `docker-compose.yml:99-100` area: add `TAILSCALE_API_URL`,
      `TAILSCALE_API_TOKEN`, `TAILSCALE_TAILNET`,
      `TAILSCALE_OAUTH_CLIENT_ID`, `TAILSCALE_OAUTH_CLIENT_SECRET`
      passthrough next to the NetBird pair
- [x] `.env.example`: add a Tailscale block (token or OAuth pair, tailnet),
      state the "exactly one provider" rule, reword "shared/NetBird
      deployments" comments to "shared/mesh deployments", and update the
      overlay name at the "Mesh overlay (docker-compose.netbird.yml)" line
- [x] `scripts/deploy-prod.sh:7`: default compose overlay →
      `docker-compose.mesh.yml`
- [x] `README.md:160-161`: update the documented compose command to
      `docker-compose.mesh.yml` in this task (not Task 6) so the repo never
      documents a file that no longer exists
- [x] `scripts/e2e.sh:25-26`: also export empty `TAILSCALE_API_TOKEN` /
      `TAILSCALE_OAUTH_CLIENT_ID` / `TAILSCALE_OAUTH_CLIENT_SECRET` so e2e
      always runs the dev resolver regardless of the host env
- [x] run `just test` and `just e2e` — must pass before task 6

### Task 6: Docs and terminology sweep

**Files:**
- Modify: `README.md`
- Modify: `server/policy.go` (comment, lines 22-23), `server/identity.go`
  (doc comments at lines 14-16, 29-31, and 232-233 — the `StaticResolver`
  "production should use NetbirdResolver" line)
- Modify: `cli/spot:300,377`, then copy the file over `sdk/spot` — it is a
  deliberate copy, not a symlink (`scripts/e2e.sh:15-19` diffs them and
  fails on drift; Caddy's mount can't follow a symlink out of `sdk/`)
- Modify: `sdk/spot.js:61`, `sdk/index.html:1071,1088`,
  `examples/demo/index.html:58`, `scripts/e2e.sh:272` (comment)

- [x] README: architecture diagram + intro say "mesh peer (NetBird or
      Tailscale)"; identity paragraph names both management APIs
- [x] README deployment section: split provider setup into two subsections —
      **NetBird**: existing access-policy + service-account PAT steps;
      **Tailscale**: ACL grant `group:employees -> spot-vm:443`, OAuth
      client with read access to devices, users, and the policy file
      (verify exact scope names against current Tailscale docs during
      implementation), `TAILSCALE_*` env vars; both reference
      `docker-compose.mesh.yml`
- [x] update remaining comment/string mentions listed in Files above to
      provider-neutral wording (groups: "a mesh group name — NetBird group
      or Tailscale ACL group")
- [x] run `just test` — comments/docs must not have broken any
      string-asserting test

### Task 7: Verify acceptance criteria

- [x] verify requirements from Overview: Tailscale deployment works
      end-to-end via stubs (resolve, groups, directory picker, policies);
      NetBird path byte-for-byte unchanged in behavior; both-set → startup
      error; auto-detect correct in all combinations
- [x] verify edge cases: tagged devices stay peer-only, IPv6 lookups return
      the canonical IPv4 `PeerIP`, non-email ACL group members handled per
      the chosen strategy, OAuth token expiry mid-flight, empty tailnet
- [x] `cd server && gofmt -l . && go vet ./...` clean
- [x] run full suite: `just test`
- [x] run `just test-integration` (needs `just up`)
- [x] run `just e2e`

### Task 8: [Final] Update documentation

- [x] README final read-through for stale NetBird-only claims
- [x] no repo CLAUDE.md exists; create only if a non-obvious pattern
      emerged (provider auto-detect rule is a candidate)
- [x] move this plan to `docs/plans/completed/`

## Technical Details

Identity mapping, NetBird ↔ Tailscale:

| Identity field | NetBird source | Tailscale source |
|---|---|---|
| `Email` | `/api/users` `email` (joined by `user_id`) | `/users` `loginName` (joined by device `user`) |
| `Name` | `/api/users` `name` | `/users` `displayName` |
| `PeerName` | `/api/peers` `name` | `/devices` `hostname` |
| `PeerIP` | `/api/peers` `ip` (single) | canonical IPv4 from `addresses[]`; all addresses map to it |
| `Groups` | peer groups ∪ user auto-groups, by name | policy-file `groups` by `loginName`, both stripped and `group:`-prefixed names |

API differences that bite:
- Tailscale responses wrap lists (`{"devices": [...]}`); NetBird returns
  bare arrays.
- Auth scheme differs: `Bearer` vs NetBird's `Token`.
- The ACL endpoint serves HuJSON by default; `Accept: application/json`
  returns plain JSON.
- `TAILSCALE_API_URL` stays configurable (tests point it at httptest;
  Headscale compatibility is NOT a goal — its API differs).
- Staleness: a brand-new device/user appears within one cache TTL (≤30s),
  identical to NetBird today.

`normalizeGroups` sketch (Task 2 — Sasha's contribution point; the marked
TODO will be in `identity_tailscale.go` with this signature):

```go
// normalizeGroups reduces raw policy-file group member lists to plain
// user logins. Tailscale forbids nested groups, but member lists may
// contain autogroup:* references or bare domains.
// Design choice: skip non-email members silently, log once per unknown
// kind, or fail the fetch? Pick and document.
func normalizeGroups(raw map[string][]string) map[string][]string
```

Provider auto-detection truth table (`newResolver` + `loadConfig`):

| NETBIRD_* | TAILSCALE_* | DEV_IDENTITY | Result |
|---|---|---|---|
| set | — | — | NetbirdResolver |
| — | set | — | TailscaleResolver |
| set | set | — | startup error |
| — | — | set | StaticResolver (`.localhost` only, as today) |
| — | — | — | nil resolver, `/api/me` 503 (as today) |

## Post-Completion

**Manual verification** (requires a real tailnet):
- create an OAuth client with read access to devices, users, and the
  policy file; confirm the scope names against current Tailscale docs
- deploy to a test VM joined to the tailnet; verify `spot.me()` returns
  email/name/peer fields, `_access.json` group allow/deny works against a
  `group:` defined in the tailnet policy, and the access picker lists
  tailnet users and groups
- verify OAuth token refresh across the 1-hour boundary (leave it running)

**External system updates**:
- spot.t1a.dev production redeploy procedure references
  `docker-compose.netbird.yml`; after this ships, the documented redeploy
  command must switch to `docker-compose.mesh.yml` (production stays on
  NetBird — no env changes needed)
- follow-up candidate (separate plan): `tailscale whois`/LocalAPI resolver
  for tokenless deployments where spot-api runs next to tailscaled
