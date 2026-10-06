# Quesma Shipper protocol

The wire contract between [Quesma Shipper](../src/) and its control plane,
[Fleet Manager](../fleet-manager/). This directory holds the protocol specification and the
contract assets both peers test against: the normative [protocol document](PROTOCOL.md), the JSON
Schemas, the configuration authority rulebook, and the golden fixtures. It contains no runnable
shipper or control plane; those live beside it in this repository and build against this directory.

The Go module embeds every asset:

```go
import protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"

raw, err := protocol.FS.ReadFile("schemas/enroll-request.schema.json")
```

The fixtures are synthetic test vectors. The token-shaped values, signatures, URLs, and the
documented Ed25519 seed are public. Do not use them as production credentials.

## Development

Install Go 1.27 or newer. The commit gate is `make check`: gofmt, go vet, the suite, and a check
that the authority table in `PROTOCOL.md` matches `authority.json` (`make gen` rewrites it).
[AGENTS.md](AGENTS.md) lists the rules for changing a released contract. Report security issues as
described in [SECURITY.md](SECURITY.md).

## Versioning and license

Releases are tags of this repository named `shipper-protocol/vX.Y.Z`; versions before the module
moved here are tags of the original
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) repository. Wire versions are
immutable after release. A breaking change gets a new endpoint and schema version. Schema `$id`
values keep the namespace the module was first published under,
`https://github.com/QuesmaOrg/shipper-protocol/`. They are stable identifiers, not download
locations. An identifier published in a release does not change in place.

Licensed under the [Apache License 2.0](LICENSE). See [`NOTICE`](NOTICE) and the repository's
[TRADEMARKS.md](../TRADEMARKS.md).
