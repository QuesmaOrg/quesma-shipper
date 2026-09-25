## Design

Design decisions are governed by
[CONSTITUTION.md](https://github.com/QuesmaOrg/quesma-shipper/blob/main/CONSTITUTION.md) in the
shipper repository — it is the constitution of the whole system, client and control plane.
[ARCHITECTURE.md](ARCHITECTURE.md) holds this module's own invariants.

## Workflow and style

The shipper repository's
[AGENTS.md](https://github.com/QuesmaOrg/quesma-shipper/blob/main/AGENTS.md) covers both, for the
client and for this service: draft PRs, comment density, when to simplify. Follow it rather than a
copy that would drift from it. What follows is only what differs here.

## Before opening a PR

Run `make check`. It needs Go 1.27 and Node 22 or newer, and it reaches the network to check
dependency licenses.

## What differs here

1. Be cautious with protocol changes, object-key changes, stored-record changes or configuration
   changes. They could break backward compatibility, and a fleet of enrolled shippers cannot be
   rolled back the way a stored record can. Test that and verify assumptions with a human.
   `src/protocol_test.go` and the wire fixtures imported from the shipper-protocol module pin the
   contract; `src/terraform_test.go` pins what the deployment templates may grant. A diff in either
   is a claim the contract should change, so read the diff and ask for confirmation.

2. Never widen a Terraform grant, or make tests less strict, without explicit confirmation. A grant
   that lets the runtime identity read below `install=` is read access to every sealed payload in
   the bucket.

3. Adding a dependency means running `make licenses` and committing the regenerated `third_party/`.
   Argue the dependency in the PR description.
