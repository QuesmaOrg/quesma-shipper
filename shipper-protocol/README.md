# Quesma Shipper protocol

The wire contract between [Quesma Shipper](https://github.com/QuesmaOrg/quesma-shipper) and its
control plane.

This repository contains the protocol specification and the contract assets that both peers test
against. It does not contain a runnable shipper or control plane. Those implementations live in
separate repositories, and the control plane repository is not public. You can inspect, test, and
propose changes to the complete wire contract here. Maintainers run the downstream contract tests
before each release.

The Go module embeds the normative [protocol document](PROTOCOL.md), the JSON Schemas, the
configuration authority rulebook, and the golden fixtures:

```go
import protocol "github.com/QuesmaOrg/shipper-protocol"

raw, err := protocol.FS.ReadFile("schemas/enroll-request.schema.json")
```

The fixtures are synthetic test vectors. The token-shaped values, signatures, URLs, and the
documented Ed25519 seed are public. Do not use them as production credentials.

## Development

Install the Go version declared in [`go.mod`](go.mod). Then run the repository checks:

```sh
go test ./...
go vet ./...
go run ./cmd/protocolgen -check
```

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before you change a released contract. Report security
issues as described in [`SECURITY.md`](SECURITY.md).

## Versioning and license

Release tags are Go module versions such as `v0.1.0`. Wire versions are immutable after release. A
breaking change gets a new endpoint and schema version. Schema `$id` values use this repository's
public namespace. They are stable identifiers, not download locations. An identifier published in
a release does not change in place.

Licensed under the [Apache License 2.0](LICENSE). See [`NOTICE`](NOTICE).

Quesma, Quesma Shipper, and the Quesma logo are trademarks of Quesma Inc. The license does not
grant trademark rights. If you distribute a modified version, read [`TRADEMARKS.md`](TRADEMARKS.md)
before you name it.
