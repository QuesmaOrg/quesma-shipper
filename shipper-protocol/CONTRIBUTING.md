# Contributing

Thank you for helping to improve the Quesma Shipper protocol. This repository is the contract
between separately maintained client and server implementations. Small edits can have
compatibility or security consequences. Participation is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Before you propose a change

- Open an issue before you make a breaking change or add a protocol capability. Describe the use
  case and the compatibility impact, not only the proposed wire shape.
- Do not change the meaning or the accepted shape of a released wire version. Add a new endpoint
  and schema version for a breaking change.
- Treat schema `$id` values as stable identifiers, not download locations. Do not change an
  identifier published in a release. A breaking change gets a new protocol version and a new
  identifier.
- Do not include production payloads, credentials, hostnames, customer data, or presigned URLs in
  issues, pull requests, or fixtures. Fixtures must be minimal synthetic examples.

## Make a change

Keep the normative document, the schemas, and the fixtures in sync. If you edit `authority.json`,
regenerate its table in `PROTOCOL.md`:

```sh
go run ./cmd/protocolgen
```

Run the same checks as CI before you open a pull request:

```sh
go test ./...
go vet ./...
go run ./cmd/protocolgen -check
```

State the compatibility and security effects in the pull request. Maintainers run the Quesma
Shipper and control-plane integration suites. External contributors do not need access to those
repositories.

## License of contributions

Unless you state otherwise, your contribution is licensed under the repository's
[Apache License 2.0](LICENSE), as described in section 5 of that license.
