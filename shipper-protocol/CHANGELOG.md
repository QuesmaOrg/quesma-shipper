# Changelog

All notable changes to this module are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow Go module semantic
versioning. Wire versions (`/v1/`, `/v2/`) are immutable and are not tracked by this file.

## [Unreleased]

### Changed

- PROTOCOL.md compatibility rules 1 and 6 now say what the schema already allows: the shipper
  ignores `/v2/uploads/authorize` response fields it does not know and validates every field it
  acts on; the server still decodes requests strictly. Documentation only, no schema or fixture
  bytes change.

### Added

- Initial public release of the protocol module: PROTOCOL.md, JSON Schemas for `/v1/enroll`,
  `/v1/config`, and `/v2/uploads/authorize`, the configuration authority rulebook, and golden
  fixtures. All assets are embedded in `protocol.FS`.
