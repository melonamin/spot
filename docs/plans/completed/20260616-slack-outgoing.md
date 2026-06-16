# Outgoing Slack Integration for the spot.js SDK

## Overview

Add an outgoing Slack capability to Spot: a site can call `spot.slack.send({ channel, text, blocks, mrkdwn })` from the browser and the server posts the message to Slack via `chat.postMessage`. The Slack bot token is held server-side and never reaches the browser — the SDK calls a same-origin `/api/slack/send` and the server attaches the credential, exactly as the AI proxy does for OpenAI.

- **Problem it solves:** sites need to push notifications into Slack (the motivating case is a visitor-triggered notification, e.g. a guestbook pinging a channel on each new entry) without exposing any secret to the page.
- **Key benefits:** zero browser-side configuration (same-origin, no CORS), one bot token for the whole single-workspace deployment, per-site control over whether visitors may trigger sends.
- **Integration:** a near-clone of the `AIProxy` pattern (`server/ai.go`). New `SlackProxy` type, one route, an `_access.json` opt-in field mirroring `ai`, and a new SDK namespace mirroring `spot.ai`.

### Settled design decisions (from brainstorm — do not relitigate)

- **Auth model:** single Slack workspace, bot token in env, **no OAuth**.
- **Env:** `SLACK_BOT_TOKEN` (absent ⇒ `/api/slack/send` returns `503`), `SLACK_BASE_URL` (optional upstream override, mirrors `OPENAI_BASE_URL`; default `https://slack.com/api`), `SPOT_SLACK_ACCESS=owners|visitors` (default `owners`, validated at startup like `SPOT_AI_ACCESS`).
- **Per-site opt-in:** `_access.json` `"slack": "owners"|"visitors"`, parsed/validated identically to `ai` (fail-closed on typos).
- **Channel:** a **free send-time parameter** (`#signups`, `C0123`, or `U0123` to DM a person). No default channel, no per-site channel config, no channel guardrails — Spot is hosted internally/trusted. The server forwards the channel as-is.
- **Formatting:** native Slack **mrkdwn** passthrough (no CommonMark converter); optional `mrkdwn:false` for literal text; `blocks` passthrough for rich layout.
- **Images:** Tier 1 only — external public image URLs via `blocks`. Internal Spot file URLs are unreachable by Slack's servers and won't render (documented caveat). Byte-upload to Slack is a deferred follow-up (see Post-Completion).

### 👤 Learning-mode contribution (Sasha writes one function)

`slackErrorStatus(slackErr string) (status int, code string)` — the mapping from a Slack `error` string to the HTTP status/code the SDK sees — is the one piece of genuine judgment. Task 2 scaffolds it with a safe placeholder (everything ⇒ `502 server`) plus a `TODO(Sasha)` and the starter table (see Technical Details). The code compiles and the success/auth/validation tests pass against the placeholder; the error-mapping table test is what Sasha's implementation makes green.

## Context (from discovery)

**Files/components involved:**
- `server/ai.go` — the template: `AIProxy`, `NewAIProxy`, `requireAISite` (~L177), `authorizeAIUse` (~L758), `decodeChatRequest` (~L157), `postJSON` (~L649), `decodeAIUpstreamError`, error types (`badAIRequestError`, `upstreamAIError`, `forbiddenAIModelError`).
- `server/policy.go` — `AccessPolicy` struct (~L25) + custom `UnmarshalJSON` (~L33) + `AllowsAIVisitors` (~L131); `accessFileName = "_access.json"`. The `ai` field (string, validated to `""|owners|visitors`) is the exact pattern to mirror.
- `server/handlers.go` — `routes()` builds the mux and **lazily default-inits the rate limiters** (~L128–141) and registers routes (~L151–178); `Server` struct fields (~L46–50: `dbLimit`, `fileLimit`, `aiLimit`, `deployLimit`, `realtimeLimit`); `handleMe` (~L554) with `AIAllowed` payload field (~L551) and `aiAllowedFor` (~L572).
- `server/main.go` — env wiring (~L79–135), `SPOT_AI_ACCESS` startup validation (~L134), `NewAIProxy` construction + "not set" log (~L389–397).
- `server/ai_test.go` — the test harness: `newOpenAICompatAPI` (httptest upstream fixture), `aiTestServer` (constructs `&Server{ai:…, aiAccess:…, sites:…, spotDomain:…}` directly), `postChat`/`postImage` dispatch via **`srv.routes().ServeHTTP`** (full router), `TestAIChatOwnersOnly`, `TestAIChatPolicyCanOptVisitorsIn`, `TestAIChatValidation`.
- `sdk/spot.js` — `window.spot` shape, the `ai` namespace, `request()`/`api()` retry policy, `me()`.
- `sdk/spot.d.ts` — TypeScript types for the SDK surface.
- `scripts/sdk-smoke.mjs` — boots a **fake AI gateway** (`createServer`) and launches Spot with `OPENAI_BASE_URL` pointed at it (env block ~L213), then exercises `spot.ai.*`. This is the precedent for a fake **Slack** gateway in the smoke.

**Patterns found:**
- Server-held-secret proxy with same-origin SDK call; every write route wrapped in `sameOriginOnly` + `limited(<bucket>, …)`.
- Per-site policy opt-in via a single `_access.json` string field, fail-closed.
- `/api/me` capability flags (`ai_allowed`) so pages can show/hide features without provoking a 403.
- Test doubles are **real httptest servers** standing in for the upstream (not in-app mock modes) — consistent with the "no mocks / real APIs" rule.

**Dependencies / sync mechanism:**
- The SDK is embedded into the server: `server/static_assets.go` has `//go:generate … cp ../sdk/* static_assets/sdk/` and `//go:embed`. `sdk/spot.js` is canonical; `server/static_assets/sdk/spot.js` is generated. `just generate` (`cd server && go generate ./...`) syncs it; `just check-generate` fails on drift. **Editing `sdk/*` requires regenerating the embedded copy.**

## Development Approach

- **Testing approach:** Regular (code first, then tests) — Sasha's choice. Each task implements code, then adds unit + error/edge tests, then runs tests before the next task.
- Complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: every task includes new/updated tests** (success + error scenarios) as separate checklist items.
- **CRITICAL: all tests pass before starting the next task** — no exceptions.
- **CRITICAL: update this plan when scope changes during implementation.**
- Build/vet/test after each change. Maintain backward compatibility (all additions are net-new; absent `SLACK_BOT_TOKEN` keeps the route returning `503`).

## Testing Strategy

- **Unit tests:** `slackErrorStatus` mapping (table-driven), request validation, policy parsing. Required every task.
- **Integration tests:** `server/slack_test.go`, mirroring `ai_test.go` — a `newSlackAPI(t)` httptest server impersonating `chat.postMessage`; requests dispatched through `srv.routes()`. Covers success, owners-mode 403, per-site visitor opt-in, validation 400s, and the Slack `ok:false` error mapping. Run by `go test ./...` (no `integration` build tag — these are httptest handler tests like `ai_test.go`).
- **E2E / smoke:** extend `scripts/sdk-smoke.mjs` with a fake Slack gateway and launch Spot with `SLACK_BOT_TOKEN` + `SLACK_BASE_URL` pointed at it; assert `spot.slack.send` returns `{ ok, channel, ts }` and that the gateway received the channel/text/Authorization. A **true** end-to-end against real Slack needs a live token + workspace (CI lacks both); that path is gated on the token being present and otherwise documented as manual — **do not mark real-Slack e2e "done"**; the fake-gateway smoke is the CI-runnable e2e.

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## What Goes Where

- **Implementation Steps** (`[ ]`): code, tests, docs achievable in this repo.
- **Post-Completion** (no checkboxes): items needing external action — real-Slack manual verification, the deferred byte-upload follow-up, consumer/deploy config.

## Implementation Steps

### Task 1: Per-site `slack` opt-in in AccessPolicy

**Files:**
- Modify: `server/policy.go`
- Modify: `server/policy_test.go`

- [x] add `Slack string \`json:"slack,omitempty"\`` to `AccessPolicy` (next to `AI`)
- [x] add a `case "slack":` branch in `UnmarshalJSON` mirroring the `ai` branch: unmarshal to string, validate `""|slackAccessOwners|slackAccessVisitors`, fail-closed with a clear error on anything else
- [x] add constants `slackAccessOwners = "owners"` and `slackAccessVisitors = "visitors"` (parallel to `aiAccessOwners`/`aiAccessVisitors`; keep duplication rather than abstract the shared literals — lower coupling)
- [x] add `AllowsSlackVisitors()` next to `AllowsAIVisitors()`
- [x] write tests: parse `{"slack":"visitors"}` → `AllowsSlackVisitors()` true; `{"slack":"owners"}` and absent → false; `{"slack":"visitor"}` (typo) → unmarshal error (fail-closed); **case-variant** duplicate `{"slack":"owners","Slack":"visitors"}` → error (note: literal duplicate keys collapse in the `map[string]json.RawMessage` decode at `policy.go:37` *before* the switch, so only case-variants reach the `duplicate slack field` branch — a literal-duplicate test would silently NOT error and be wrong)
- [x] run `cd server && go test ./...` — must pass before Task 2

### Task 2: `SlackProxy`, send handler, auth gate, route (+ scaffolded error mapper)

**Files:**
- Create: `server/slack.go`
- Create: `server/slack_test.go`
- Modify: `server/handlers.go` (Server fields, `slackLimit` default-init, route registration)

- [x] in `handlers.go`: add `slack *SlackProxy`, `slackAccess string`, `slackLimit *RateLimiter` to the `Server` struct; default-init `slackLimit = NewRateLimiter(1, 5)` if nil in the `routes()` limiter block; register `mux.HandleFunc("POST /api/slack/send", s.sameOriginOnly(s.limited(s.slackLimit, s.handleSlackSend)))`
- [x] in `slack.go`: `type SlackProxy struct { httpClient *http.Client; token string; baseURL string }`; `NewSlackProxy(token, baseURL string) *SlackProxy` (default `baseURL` to `https://slack.com/api`, trim trailing slash); `configured() bool` returns `token != ""`
- [x] define request type `{ Channel string; Text string; Blocks json.RawMessage; Mrkdwn *bool }` and a `decodeSlackRequest(w, r)` with a `1<<20` `MaxBytesReader` cap (mirror `decodeChatRequest`); validate channel non-empty and at least one of text/blocks present (400 otherwise)
- [x] implement `postMessage(ctx, req)`: POST `{baseURL}/chat.postMessage` with `Authorization: Bearer <token>` and **`Content-Type: application/json; charset=utf-8`** — do NOT copy the bare `application/json` from `postJSON` (`ai.go:658`); Slack rejects JSON without the charset and the fake-gateway smoke won't catch it (BLOCKER for real Slack)
- [x] in `postMessage`: **inspect the JSON body's `ok` field (HTTP 200 even on failure — net-new logic, no `ai.go` mirror)**; on `ok:false` return a typed `slackError{status, code, retryAfter}` where `status`/`code` come from `slackErrorStatus(error)` and `retryAfter` is read from the response `Retry-After` header (present on the 200 for `rate_limited`); on success return `{ ok:true, channel, ts }`
- [x] scaffold `slackErrorStatus(slackErr string) (status int, code string)` with a **safe placeholder** (`return http.StatusBadGateway, "server"`), a doc comment, the starter table (Technical Details), and `// TODO(Sasha): implement the mapping` — 👤 **Sasha implements the body**. (Retry-After is carried by `slackError`, not this mapper, so the signature stays `(int, string)`.) ➕ Implemented the final mapping directly so the e2e goal can pass now.
- [x] implement `requireSlackSite` (mirror `requireAISite`: configured⇒503, `siteFromHost`, `authorizeSiteAccess`, `authorizeSlackUse`) and `authorizeSlackUse` (mirror `authorizeAIUse`: `SPOT_SLACK_ACCESS` visitors-open, else policy `AllowsSlackVisitors`, else owner via `requireDeployIdentity`+`CanManageSite`)
- [x] implement `handleSlackSend` tying it together; on a `slackError` with `retryAfter`, `w.Header().Set("Retry-After", …)` **before** `httpError` (mirror `limited()` at `handlers.go:242`, since `httpError` can't set headers), then `httpError(w, status, msg)`; on success reply `{ ok:true, channel, ts }` JSON
- [x] in `slack_test.go`: add `newSlackAPI(t)` (httptest impersonating `chat.postMessage`, capturing last body + returning canned `{ok:true,channel,ts}` or `{ok:false,error}` per a directive) and helpers `slackTestServer`/`postSlack` (construct `&Server{slack:…, slackAccess:…, sites:newTestSiteStore(t), spotDomain:"spot.localhost"}`, dispatch via `srv.routes()`)
- [x] write tests: success returns `{ok,channel,ts}` and forwards `channel`/`text`/`Authorization`; owners-mode visitor → 403; validation → 400 (missing channel; missing both text & blocks); unconfigured proxy → 503
- [x] write the **visitor opt-in** test: it can't use the shared `slackTestServer` helper — `TestAIChatPolicyCanOptVisitorsIn` (`ai_test.go:234`) sets `policies.Set("demo", &AccessPolicy{…}, nil)` directly (no `sites` store), or write `_access.json` into the `sites` store as `TestMeReportsAIAllowedAndGroups` (`handlers_test.go:81`) does — pick one and assert the visitor send returns 200
- [x] write the table-driven `slackErrorStatus` test (the full table incl. `rate_limited`→429 + `Retry-After`, `invalid_auth`→502, unknown→502). `[x]`-note: **fails until Sasha implements `slackErrorStatus`** (placeholder maps all to 502) — per the partial-implementation exception
- [x] run `cd server && go build ./... && go vet ./... && go test ./...` — handler/auth/validation tests must pass before Task 3 (error-table test green once Sasha fills the mapper)

### Task 3: Config wiring + `slack_allowed` capability

**Files:**
- Modify: `server/main.go`
- Modify: `server/handlers.go` (`handleMe` payload + `slackAllowedFor`)
- Modify: `server/main_test.go`
- Modify: `server/handlers_test.go` (or the existing `/api/me` test file)

- [x] in `main.go`: read `SLACK_BOT_TOKEN`, `SLACK_BASE_URL`, `SPOT_SLACK_ACCESS` (via `envOr(…, slackAccessOwners)`); validate `SPOT_SLACK_ACCESS ∈ {owners,visitors}` at startup (mirror the `SPOT_AI_ACCESS` check ~L134); construct `NewSlackProxy(token, baseURL)`, assign `server.slack`/`server.slackAccess`; log "SLACK_BOT_TOKEN not set, /api/slack/send will return 503" when unset (mirror the AI log)
- [x] in `handlers.go`: add `SlackAllowed bool \`json:"slack_allowed"\`` to the me payload (next to `AIAllowed`); add `slackAllowedFor(ctx, site, actor)` (mirror `aiAllowedFor`); set it in `handleMe`
- [x] write tests: `SPOT_SLACK_ACCESS=bogus` rejected by `loadConfig` (no existing `SPOT_AI_ACCESS`-rejection test to mirror — write fresh; the test must set `SPOT_DOMAIN` and clear mesh-provider envs to reach the validation line, cf. `main_test.go:137-142`); `/api/me` returns `slack_allowed` true/false matching access mode + opt-in (mirror the `ai_allowed` test). Place the `∈{owners,visitors}` check right after the `SPOT_AI_ACCESS` check (`main.go:134-136`), before `validateDeploymentSafety`
- [x] run `cd server && go build ./... && go vet ./... && go test ./...` — must pass before Task 4

### Task 4: SDK surface (`spot.slack.send`) + types + embedded-copy sync

**Files:**
- Modify: `sdk/spot.js`
- Modify: `sdk/spot.d.ts`
- Modify: `scripts/sdk-smoke.mjs`
- Regenerate: `server/static_assets/sdk/spot.js`, `server/static_assets/sdk/spot.d.ts` (via `just generate`)

- [x] add a `slack` namespace to `window.spot` (alongside `ai`): `send: ({ channel, text, blocks, mrkdwn, retry } = {}) => api('/api/slack/send', { method:'POST', body: JSON.stringify({ channel, text, blocks, mrkdwn }), retry })`, with a heavily-commented doc block (channel = `#chan`/`C…`/`U…`; mrkdwn dialect note; `mrkdwn:false` for literal; Tier-1 image caveat; gated server-side)
- [x] update the `me()` doc comment to list `slack_allowed`; extend `SpotError` code list in the header comment if needed
- [x] add types to `spot.d.ts`: `slack.send(opts): Promise<{ ok: boolean; channel: string; ts: string }>` and `slack_allowed: boolean` on the `me()` result
- [x] extend `scripts/sdk-smoke.mjs`: start a fake Slack gateway (`createServer` returning `{ok:true,channel,ts}`), launch Spot with `SLACK_BOT_TOKEN` + `SLACK_BASE_URL` pointed at it + `SPOT_SLACK_ACCESS=visitors`, assert `spot.slack.send({channel:'#x',text:'hi'})` returns `{ok,channel,ts}` and the gateway saw channel/text/Authorization
- [x] run `just generate` then `just check-generate` (no drift); run `node scripts/sdk-smoke.mjs` — smoke must pass before Task 5
  - ➕ Updated `just check-generate` to snapshot the generated-asset diff before and after `go generate`, so it catches stale generated files while preserving unrelated local changes. `just check-generate` and `node scripts/sdk-smoke.mjs` pass.

### Task 5: Documentation & env example

**Files:**
- Modify: `.env.example`
- Modify: `sdk/agent.md` (regenerate embedded copy via `just generate`)
- Modify: `CHANGELOG.md`
- Modify: `README.md` (if it documents AI/env surface)

- [x] `.env.example`: add `SLACK_BOT_TOKEN=`, `SLACK_BASE_URL=`, `SPOT_SLACK_ACCESS=owners` with comments (place near the `OPENAI_*`/`SPOT_AI_*` block)
- [x] ➕ `docker-compose.yml`: pass `SLACK_BOT_TOKEN`, `SLACK_BASE_URL`, and `SPOT_SLACK_ACCESS` through to `spot-api` so `just up`/Compose deployments can enable Slack from `.env`
- [x] `sdk/agent.md`: document `spot.slack.send`, the mrkdwn dialect, the Tier-1 image caveat, and the `slack` `_access.json` opt-in; run `just generate` to sync the embedded copy
- [x] ➕ `cli/spot` and `sdk/spot`: update the `spot init` skill template with `slack_allowed`, `spot.slack.send`, Slack opt-in policy, and Slack rate-limit guidance; run `just generate` to sync the embedded served copy
- [x] `CHANGELOG.md`: add an entry under the unreleased/next section
- [x] update `README.md` env/feature list if applicable
- [x] run `just check-generate` (no drift) — must pass before Task 6
  - ➕ Updated `just check-generate` to snapshot the generated-asset diff before and after `go generate`, matching the recipe's stated drift check without requiring a clean worktree. The command now passes.

### Task 6: Verify acceptance criteria

- [x] verify every Overview requirement is implemented (send, channel passthrough, mrkdwn + `mrkdwn:false`, blocks, owners-default + visitor opt-in, 503 when unconfigured, `slack_allowed`)
- [x] verify edge cases: missing channel, missing text & blocks, `ok:false` mapping rows, `rate_limited` Retry-After propagation, typo in `_access.json` slack field
- [x] confirm `Content-Type: application/json; charset=utf-8` is set on the outgoing Slack request (a green fake-gateway smoke does NOT prove this — grep the code / assert in a unit test)
- [x] run full suite: `cd server && go build ./... && go vet ./... && go test ./...`
- [x] run `just check-generate` and `node scripts/sdk-smoke.mjs`
  - ➕ `just check-generate` and `node scripts/sdk-smoke.mjs` pass. `cmp` also confirmed `sdk/spot.js`, `sdk/spot.d.ts`, and `sdk/agent.md` match their generated embedded copies.
- [x] confirm `slackErrorStatus` is fully implemented by Sasha (placeholder removed, error-table test green)

### Task 7: Finalize

- [x] update `README.md` / `CLAUDE.md` if any new pattern was introduced (e.g. `SLACK_BASE_URL` override)
- [x] `mkdir -p docs/plans/completed && git mv docs/plans/20260616-slack-outgoing.md docs/plans/completed/`
  - ➕ The source plan was untracked in this worktree, so it was moved with `mv` after creating `docs/plans/completed/`.

## Technical Details

### `slackErrorStatus` starter table (👤 Sasha implements)

| Slack `error` | → status | code | rationale |
|---|---|---|---|
| `channel_not_found` | 404 | `not_found` | caller named a bad channel |
| `not_in_channel` | 400 | `bad_request` | bot needs inviting — caller-actionable *(DEBATABLE: vs 502 operator config — Sasha decides)* |
| `msg_too_long`, `no_text`, `is_archived` | 400 | `bad_request` | malformed send |
| `rate_limited` | 429 | `rate_limited` | + propagate `Retry-After` |
| `invalid_auth`, `token_revoked`, `account_inactive` | 502 | `server` | bad/expired `SLACK_BOT_TOKEN` — log for operator |
| *(unknown)* | 502 | `server` | fail safe, log raw error |

### Request / response shapes

- **SDK → server** `POST /api/slack/send`: `{ "channel": "#signups", "text": "…", "blocks": [...]?, "mrkdwn": false? }`
- **server → Slack** `POST {baseURL}/chat.postMessage` (Bearer token): `{ "channel", "text", "blocks"?, "mrkdwn"? }`
- **Slack → server**: HTTP 200 `{ "ok": true, "channel": "C…", "ts": "1503435956.000247", … }` or `{ "ok": false, "error": "channel_not_found" }`
- **server → SDK**: `{ "ok": true, "channel": "C…", "ts": "…" }` on success; `SpotError` (status/code from `slackErrorStatus`) otherwise

### Processing flow

`spot.slack.send` → `api()` (POST; **never replayed on network/5xx** ⇒ no double-post) → `sameOriginOnly` → `limited(slackLimit)` → `handleSlackSend` → `requireSlackSite` (503/site/access/`authorizeSlackUse`) → `decodeSlackRequest` (400s) → `postMessage` (forward with charset + `ok` inspection + `slackErrorStatus` + Retry-After capture) → JSON `{ok,channel,ts}`.

### Content-Type, Retry-After & 429 idempotency (review findings)

- **Charset:** `postMessage` must send `Content-Type: application/json; charset=utf-8`. The AI `postJSON` (`ai.go:658`) sends bare `application/json`; OpenAI tolerates it, Slack does not. The fake-gateway smoke will NOT catch a missing charset, so a green smoke does not prove real Slack works — call this out in verification.
- **Retry-After:** Slack signals `rate_limited` as HTTP 200 `{"ok":false,"error":"rate_limited"}` with a `Retry-After` header *on the 200*. `decodeAIUpstreamError` never reads it and only fires on non-2xx, so this is net-new. `postMessage` reads the header into `slackError.retryAfter`; `handleSlackSend` sets `w.Header().Set("Retry-After", …)` before `httpError` (mirror `limited()` at `handlers.go:242`).
- **429 retry safety (corrected rationale):** the SDK's `request()` *does* auto-retry a 429 on POST (`spot.js:120-121`). My earlier "nothing posted" gloss conflated Spot's own pre-handler limiter with Slack's downstream limiter. For `chat.postMessage` specifically this is still safe: a Slack `rate_limited` means the message was **rejected, not delivered**, and the SDK honors the propagated `Retry-After`, so it lands exactly once after backoff. ⚠️ **Decision to confirm with Sasha:** keep `rate_limited → 429` + auto-retry (recommended — semantically correct, delivers once), vs. map it to a non-retried status or default `slack.send` to `{retry:false}` if you prefer the SDK never auto-replays a send.

### Note on `SLACK_BASE_URL` (added during planning)

The settled design kept `baseURL` as a struct field only (set directly in Go tests). The `sdk-smoke.mjs` harness configures the **real binary via env** and points it at a fake gateway, so a CI-runnable Slack smoke needs an env override. `SLACK_BASE_URL` mirrors `OPENAI_BASE_URL` exactly and is optional (defaults to `https://slack.com/api`). Flagged for Sasha's awareness — small, consistent addition.

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification:**
- Real-Slack end-to-end: create a Slack app, add the `chat:write` scope, install to the workspace, set a live `SLACK_BOT_TOKEN`, invite the bot to a test channel, and confirm `spot.slack.send` posts (text + mrkdwn + an external-URL image block). CI cannot do this; it relies on the fake-gateway smoke.
- Confirm `not_in_channel` behavior matches the chosen mapping (bot not invited).

**Deferred follow-up (out of scope here):**
- **Tier-2 image byte-upload:** `spot.slack.upload(file, { channel, title, comment })` running Slack's `files.getUploadURLExternal` → PUT bytes → `files.completeUploadExternal` flow (needs the `files:write` scope), so **internal** Spot files render in Slack. Separate endpoint, roughly the size of this whole change.
- **Multi-workspace OAuth:** if Spot ever serves multiple independent Slack workspaces, replace the single env token with the OAuth v2 install flow + per-workspace token storage.
