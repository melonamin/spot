# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.2.0]: https://github.com/melonamin/spot/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/melonamin/spot/releases/tag/v0.1.0
