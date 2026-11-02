## Design

Design decisions are governed by the repository's [CONSTITUTION.md](../CONSTITUTION.md). The
normative contract is [PROTOCOL.md](PROTOCOL.md); the schemas, the authority rulebook and the
fixtures under this directory are what both peers test against.

## Workflow and style

The repository's [AGENTS.md](../AGENTS.md) covers both, for every component. What follows is only
what differs here.

## Before opening a PR

Run `make check`. It needs Go 1.27 or newer and nothing else.

## What differs here

1. A released wire version is immutable. Never change the meaning or the accepted shape of
   `/v1/` or `/v2/`; a breaking change is a new endpoint and schema version. A change to a schema,
   a fixture or `authority.json` is a contract claim: state the compatibility effect in the PR, and
   keep `PROTOCOL.md` in step (`make gen` rewrites its authority table).

2. Schema `$id` values are stable identifiers, not download locations. They keep the
   `https://github.com/QuesmaOrg/shipper-protocol/` namespace the module was first published under,
   and an identifier published in a release does not change in place.

3. The shipper (`src/`) and Fleet Manager (`fleet-manager/`) build against this directory, so a
   change here runs their contract tests in the same PR. The fixtures are golden bytes for both:
   a diff in one of them is a claim the wire should change, so read it and ask before updating.
   Record every user-visible change in `CHANGELOG.md`; a release is a tag `shipper-protocol/vX.Y.Z`.

4. Fixtures are synthetic. No production payloads, credentials, hostnames or presigned URLs.
