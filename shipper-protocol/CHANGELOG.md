# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow Go module semantic
versioning. Wire versions (`/v1/`, `/v2/`) are immutable and are not tracked by this file.

## [Unreleased]

### Changed

- The shipper resends an upload authorization or a PUT once, after about one second, when no
  complete HTTP response arrived (DNS, connect, TLS handshake, reset, timeout, a 2xx body cut
  short). A second silence stops the run's uploads as offline. HTTP statuses are still never
  retried, and the heartbeat stays one attempt. A server can see each authorization and PUT twice
  where it saw it once, so up to four of each naming one object per run when a ticket also
  expires (was two); PUTs were already unconditional onto a versioned bucket. No schema or fixture
  bytes change.

- Document the existing `403` body `this install is revoked` as the stable install-revocation
  signal on device-authenticated calls. Other 403s do not revoke an install locally. No response,
  schema or fixture bytes change.
- Grant enrollment accepts a valid replacement claim for an active install with the same
  organization, install ID, device key and age recipient, without rewriting the original record.
  Invite replay remains exact. Request/response schemas and fixtures are unchanged. Deploy the
  server support before clients rotate pending grants; older servers may return 409.

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

- `POST /v3/uploads/authorize` accepts an explicit `force_resend: true` and issues PUT tickets
  without consulting stored hashes. Its signature binds the v3 endpoint. Existing v1/v2 shapes,
  signatures and behavior are unchanged; normal clients keep using v2. Upgrade Fleet Manager
  before using the local force-resend option. Unsupported servers cause an error, never a v2 fallback.

- `sources[].exclude_add` unions relative exclusion globs after all replacement layers.
  Globs use forward-slash separators on every platform; backslashes, a leading `~` or `$`, and
  empty, `.` or `..` segments are rejected.
  `sources.exclude_add` capability negotiation and the shared reduction disable affected sources
  for older readers; the two-copy cache preserves that behavior through offline downgrades, and
  its `config_full_features` keeps a build lacking a later feature on the reduced copy.

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
