# Architecture

The repository's [CONSTITUTION.md](../CONSTITUTION.md) governs what this service assumes. This document governs how its code is arranged, and it is short for a
reason: fleet-manager is one `package main` under `src/`, with no internal packages and no import
graph to police. There is nothing here resembling the shipper's layering table.

What it has instead is a small set of invariants that outrank any local design choice, each with a
test that holds it.

## Shape

```
src/
  main.go           flags, environment, the one --serve command
  server.go         the shipper-facing routes and the device auth in front of them
  admin_http.go     /v1/admin, and the two credential scopes
  ui.go             /admin/, serving the embedded assets in admin-ui/
  manager.go        organizations, enrollment, revocation
  model.go          the records, and what a valid one is
  upload.go         turning an upload request into a batch of tickets
  seen.go tags.go   the two disposable, off-critical-path records
  telemetry*.go     the forwarding proxy and the deployment signing identity
  health.go         collection-health reports from a reporter credential
  store.go          ObjectStore -- the only durable-state primitive
  aws.go gcp.go azure.go   one implementation each, plus the presigner
  admin-ui/         plain HTML, CSS and JavaScript, embedded in the binary
terraform/          what a deployment applies; the grants are part of the design
```

## The invariants

**One durable-state primitive.** `ObjectStore` in `src/store.go` is it. There is no database, no
cache and no local disk: a replica can be killed at any point and the next request is served
correctly by another. Anything that needs to survive a restart is an object in the bucket.

**Only the provider files know a cloud.** `aws.go`, `gcp.go` and `azure.go` implement `ObjectStore`
and `UploadSigner`. No other non-test file imports a cloud SDK, which is what keeps every other file
testable against an in-memory store and what makes a fourth provider a new file rather than a
change to the service.

**The service never reads a trajectory.** It has no decrypt path, and it holds no identity that
could decrypt one. The single object it reads below `install=` is `tags.json`, the human name for
an install, and the templates grant read on that one object name rather than on the prefix —
because the prefix is every sealed payload in the bucket.

**There is no delete.** No route deletes an object, and no organization can be removed. GCS is
granted `storage.objects.delete` only because replacing object bytes there requires
delete-plus-create; bucket versioning preserves what is displaced.

**Control records are conditional.** A record is created with `Create` (`If-None-Match: *`) and
updated with `Replace` (`If-Match`). Replicas coordinate through the store and in no other way,
which is why the deployment has no leader, no lock and no coordination service — and why object
versioning is checked at startup and refused if absent.

**Disposable records are unconditional, and never fail a request.** `seen/` and `tags.json` use
`Put`, are tagged `lifecycle=ephemeral` so their superseded versions can be expired, and collapse
repeated check-ins within a minute into one write. A failed write of one is logged and dropped. The
hot path must never rewrite a security record.

**The ticket contract is closed.** A presigned upload ticket may carry `x-amz-tagging` and
`x-amz-meta-*` and nothing else; `s3TicketHeaders` in `src/aws.go` refuses any other signed header
rather than passing it through. A ticket names one key, and that key is inside the requesting
install's own prefix.

**The templates are part of the design.** `terraform/aws` and `terraform/gcp` are not deployment
convenience — they are where the least-privilege argument is actually made.
`src/terraform_test.go` pins what they must contain and what they must not, so widening a grant
fails the suite rather than passing review unnoticed.

## Where a change goes

A new shipper-facing behaviour starts in `shipper-protocol`, is released as a module version, and
arrives here as a dependency bump plus a handler. A new storage backend is a new provider file and
nothing else. A new administrative operation is a route in `admin_http.go`, a method on `Manager`,
and a decision about which credential scope may call it — the reporter credential exists so that
whatever reports collection health cannot also create organizations or revoke installs.

A change that needs a payload, a database, a lock, or a delete is not a fleet-manager change. It is
a constitutional one.
