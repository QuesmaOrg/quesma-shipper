// Package protocol exposes the authoritative Quesma Shipper wire-protocol
// documentation, rulebook, JSON Schemas, and golden fixtures.
package protocol

import "embed"

// FS contains the normative protocol document, authority rulebook, and every schema and fixture.
// Paths are rooted at this module, for example
// "schemas/enroll-request.schema.json" and
// "fixtures/v1/enroll/request.json".
//
//go:embed PROTOCOL.md authority.json schemas fixtures
var FS embed.FS
