# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added named, repository-scoped publishing keys for off-mesh CI deploys. Keys can create and update sites within a fixed prefix, carry publisher attribution, and can be managed and revoked from `/spots`.
- Upgraded Spot Show with structural validation, safe local image bundling, stable card links, system/light/dark appearance, fullscreen Mermaid and image views, theme-aware sandboxed HTML, code line numbers and highlighting, ANSI terminal output, split diffs, and agent trace timelines.

### Fixed

- Replaced Spot Show's direct sandbox document inspection with a guarded resize and theme bridge so opaque-origin HTML blocks size correctly without `allow-same-origin`.
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

[Unreleased]: https://github.com/melonamin/spot/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/melonamin/spot/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/melonamin/spot/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/melonamin/spot/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/melonamin/spot/releases/tag/v0.1.0
