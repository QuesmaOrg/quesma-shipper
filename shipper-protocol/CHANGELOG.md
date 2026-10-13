# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow Go module semantic
versioning. Wire versions (`/v1/`, `/v2/`) are immutable and are not tracked by this file.

## [Unreleased]

### Changed

- The module moved into the [quesma-shipper](https://github.com/QuesmaOrg/quesma-shipper)
  repository as `shipper-protocol/`. Its import path is now
  `github.com/QuesmaOrg/quesma-shipper/shipper-protocol` and releases are tags named
  `shipper-protocol/vX.Y.Z`. Schema `$id` values, the schemas and the fixture bytes are unchanged.
- The module requires Go 1.27.0 (was 1.25.1), the version the shipper builds with.
- The rulebook's `drain_deadline` default is 5m, the value the shipper uses; it said 1h.
- PROTOCOL.md compatibility rules 1 and 6 now say what the schema already allows: the shipper
  ignores `/v2/uploads/authorize` response fields it does not know and validates every field it
  acts on; the server still decodes requests strictly. Documentation only, no schema or fixture
  bytes change.

### Added

- `/v1/config` requests may carry `catalog`: the build's compiled source catalog (sources, rule
  packs, served-document features), root templates unexpanded. Optional, so older clients are
  served as before. PROTOCOL.md gains "Source catalog report": the reported sources are what a
  control plane has discovered, not an allowlist; how it validates writes and serves per install
  with them; and that it never leaves a requested rule pack out of a served document.

## [0.1.0] - 2026-09-04

### Added

- Initial public release of the protocol module: PROTOCOL.md, JSON Schemas for `/v1/enroll`,
  `/v1/config`, and `/v2/uploads/authorize`, the configuration authority rulebook, and golden
  fixtures. All assets are embedded in `protocol.FS`.

[0.1.0]: https://github.com/QuesmaOrg/shipper-protocol/releases/tag/v0.1.0
