## What changes

<!-- One or two sentences. Name the endpoint, schema, fixture, or document you changed. -->

## Compatibility

<!-- Answer each question. See CONTRIBUTING.md. -->

- Does this change the accepted shape or meaning of a released wire version? (must be "no")
- Does this add a new endpoint or schema version?
- Which peer must ship first: client, server, or neither?

## Security

<!-- State the security effect, or write "none". -->

## Checklist

- [ ] `go test ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go run ./cmd/protocolgen -check` passes
- [ ] PROTOCOL.md, schemas, and fixtures are changed together
- [ ] Fixtures contain only synthetic data
