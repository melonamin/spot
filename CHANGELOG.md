# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- A restricted site's pages, files and source download are sent with `Cache-Control: no-store`, so a caching proxy in front of Spot cannot serve one viewer's content to another.

### Changed

- Deleting a site now always keeps its name reserved for the original owner, and only a platform admin can release a deleted name; before, an owner or admin deletion freed the name for anyone, and a new owner inherited an origin where the old site's pages or service workers could still run.

## [0.6.1] - 2026-09-29

### Added

- Added `preserve_access=require` to deploys: it keeps the stored access policy like `true`, but answers `409` when there is none to keep (a new or inactive site, or an active site without a policy). (#23)

### Changed

- `/api/sites/stats` now requires an identified caller, like the other site listings. (#23)
- An API request whose caller no identity matches now answers `401` instead of `404`. (#23)

### Fixed

- A site's `_access.json`, in any letter case, is no longer served to visitors or included in its source download or Cloudflare export. (#23)
- A path below a file on local storage (such as `/page.html/`) answers `404` instead of `500`. (#23)
- A deploy now authenticates the caller before reading the upload, so an anonymous request cannot make the server buffer up to 100 MiB. (#23)

## [0.6.0] - 2026-09-29

### Added

- Added delegated login (`SPOT_LOGIN_URL`) so an embedding application can sign viewers in to restricted sites with short single-use tokens and per-site session cookies, plus `SPOT_FRAME_ANCESTORS`, `SPOT_APEX_REDIRECT_URL`, and a Caddy on-demand TLS check at `/api/tls/ask`. (#22)
- Added `PUT /api/sites/{name}/access` to change a site's access policy without redeploying, `GET /api/sites/visible`, and `allow` in site listings. (#22)

### Changed

- A site update that ships no `_access.json` now keeps the stored access policy instead of removing it; send `preserve_access=false` (`spot deploy --replace-access`, or Clear in the web deployer) to remove it.
- A site host serves only its own site's uploads (`/api/files/<other-site>/…` answers `404`) in every identity mode, because the visitor's identity there would otherwise let one site read another's restricted uploads. The apex still serves any site's uploads.
- Delegated login binds each sign-in to the browser that started it: the login app must copy the redirect's `state` into the token's `state` claim. Open-site pages start a sign-in at `/api/auth/login`, sign-out requires a page load, and delegated login requires HTTPS or `*.localhost` and always uses `__Host-` cookies.
- Formatted storage usage on the stats page with human-readable units.

### Fixed

- Kept the homepage GitHub link aligned with the other header actions. (#21)

## [0.5.0] - 2026-08-28

### Added

- Added named, repository-scoped publishing keys for off-mesh CI deploys. Keys can create and update sites within a fixed prefix, carry publisher attribution, and can be managed and revoked from `/spots`. (#20)
- Upgraded Spot Show with structural validation, safe local image bundling, stable card links, system/light/dark appearance, fullscreen Mermaid and image views, theme-aware sandboxed HTML, code line numbers and highlighting, ANSI terminal output, split diffs, and agent trace timelines. (#18)

### Fixed

- Stopped `spot deploy --screenshot` and `spot show deploy` from hanging forever when the browser writes the capture but never exits, preferred `chrome-headless-shell` for one-shot capture, and gave deferred page work (Mermaid, syntax highlighting) a virtual time budget so thumbnails are no longer captured mid-render. (#19)
- Replaced Spot Show's direct sandbox document inspection with a guarded resize and theme bridge so opaque-origin HTML blocks size correctly without `allow-same-origin`. (#18)
- Displayed NetBird peer names instead of IP addresses for sites owned by setup-key CI or server peers, including existing sites after their next owner deploy.

## [0.4.0] - 2026-07-18

### Added

- Added optional Cloudflare Pages publishing with public or email-restricted access, durable ownership and recovery state, reload-safe publication jobs, and management controls in `/spots`. (#14, #15)
- Added `_access.json` maintainer delegation, manageable-site APIs, and owner-recoverable tombstones while preserving the original owner's permanent recovery claim. (#17)
- Added Spot Show commands for building, deploying, and watching report sites with Markdown, Mermaid, diffs, terminal output, JSON, images, and sandboxed HTML. (#10)
- Added a public, aggregate-only `/stats` page for Spot growth, activity, freshness, tags, and preview coverage. (#11)
- Added a responsive `/help` field guide with deep-linked workflows, release notes, and agent setup guidance. (#16)
- Added automatic gallery preview capture for Spot Show deploys and a saved-spots gallery filter.
- Added request-aware `/agent.md` instructions and a full-instructions copy action that does not require agents to fetch an external URL. (#13)

### Fixed

- Preserved uploaded files' original content types when serving them.
- Improved gallery cards with separate author attribution, stable row sizing, and safer text wrapping across viewport widths.
- Bounded generated Cloudflare Pages project names to the provider's length limit. (#15)

## [0.3.0] - 2026-06-23

### Added

- Browser SDK and server proxy support for `spot.slack.send`, backed by a server-side Slack bot token and per-site visitor opt-in.
- Forward-auth identity support for deployments behind an authenticating reverse proxy. (#7)
- Gallery metadata support via `_spot.json`, HTML title/description extraction, public site tag chips, and tag-aware gallery search/filtering.
- Optional AI tag suggestions for public sites that do not provide explicit gallery tags.
- Maintenance command to backfill existing sites with gallery metadata, `_spot.json`, and optional screenshots without redeploying or changing ownership.

### Fixed

- Hardened deploys that add, remove, or broaden `_access.json` policies so metadata and policy caches remain fail-closed if storage updates fail.
- Fixed gallery title sorting and source-download controls for titled sites and touch devices.

## [0.2.0] - 2026-06-15

### Added

- Browser SDK: streaming AI chat, document ownership, and file `list`/`delete`. (#6)
- Browser SDK: document-store queries, atomic counter increment, typed errors, and automatic retry. (#6)
- Database: owned mutations and cursor-based pagination. (#6)

### Fixed

- Hardened the create/query/stream request paths and deduplicated shared logic.
- Hardened SDK retry and replay behavior.
- Validated null filters and counter values on the server.
- Hardened document ownership and streaming behavior across the API.
- Rejected incomplete AI chat streams instead of returning partial results.

## [0.1.0]

- First tagged release: prebuilt multi-arch images and CI/release pipeline.

[Unreleased]: https://github.com/melonamin/spot/compare/v0.6.1...HEAD
[0.6.1]: https://github.com/melonamin/spot/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/melonamin/spot/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/melonamin/spot/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/melonamin/spot/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/melonamin/spot/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/melonamin/spot/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/melonamin/spot/releases/tag/v0.1.0
