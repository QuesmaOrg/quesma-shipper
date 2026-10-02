# Fleet Manager gotchas

Facts a reviewer had to supply in a past review, grouped by where they bite. Each one cost a review
round; knowing it lets the review start from the fix. Numbers are quesma-shipper pull requests
where the fact surfaced; facts learned before the module moved into this repository carry none.

## Cloud storage and IAM

- S3 answers a GetObject on a missing key with 403, not 404, unless the caller may `s3:ListBucket`
  on that key. A read of a maybe-absent object needs a `s3:ListBucket` statement with an
  `s3:prefix` condition shaped exactly like the key; the `InstallAbsence` statements in the AWS
  template are the pattern. GCS returns 404. (#74)
- S3 and GCS authorize HEAD as a full read; there is no metadata-only permission. A dedup probe that
  HEADs mirror objects is read access to every sealed payload, which is why the templates grant
  read on the prefix and why the service still cannot open one. (#74)
- The GCS client wraps a not-found error, so `==` and type assertions never match it and every
  absent object looks like a storage failure. Match with `errors.Is` against
  `storage.ErrObjectNotExist`; `mapGCSError` does it. (#74)
- Every control record lives under `v1/`, so a reader allowed `v1/*` reads invites, grants and
  credential digests. A test that looks for a `/control/` string in a grant passes for the wrong
  reason, because the grant never contained it.
- The external reader grant is current versions only; the ETL reads `control/config.json` for
  display names, so that object stays readable.
- `tags.json` sits in the install's own root, not under `control/`, so a bucket walker resolves an
  id without the service and without a database credential; its read grant names the single
  object. Putting personal data into it widens what that grant discloses to everyone holding it.
  (#65)
- The telemetry signing seed lives at `private/fleet-manager/telemetry-identity.json`, outside
  `v1/`, so `v1/*` readers cannot see it. The runtime needs read and write on it at startup even
  when forwarding is off; archive mirrors exclude `private/`.
- Object versioning is required and checked at first organization creation, not at startup.
  Replacing an object creates a new version; a secret written by mistake lives on in the old one.
  (#79)

## Terraform templates and deployment

- The AWS module reads `/v1/telemetry/public-key` through `data "http"` with a postcondition, so an
  apply against an image older than that endpoint fails on a telemetry message that never mentions
  the pin.
- A committed production pin can drift from what runs. A pin bump names a tag that exists on Docker
  Hub and contains the change, and the comment beside it says which repository's commit it is.
- Both templates default to `docker.io/quesma/fleet-manager:latest`, so a push of `latest` is a
  customer deploy. The publish job needs an environment for reviewers to engage, and an old
  publisher must be switched off at cutover or two pipelines race on `latest`. (#52)
- ECS Express Mode needs two public subnets in different availability zones with free addresses in
  each, and the AWS provider can report an inconsistent plan unless the container command is set
  explicitly. IAM role-name prefixes are capped, hence the short module name. (#52)
- `.terraform.lock.hcl` files are committed on purpose. OpenTofu generates them; Terraform rewrites
  them on init.
- A Terraform-only change still needs a deploy step, and the description says what the plan should
  change and nothing else.

## Stored records and protocol

- Older binaries decode `tags.json` strictly: a replica still on the old binary drops the name of
  any install whose record carries a new field and cannot rename it. Upgrade every replica before
  writing the new field. The same held for a new telemetry configuration field. (#65)
- `seen/` writes collapse to one per `seenWriteInterval`, so an observed quiet period trails the
  real one by up to that interval.
- Enrollment completion checks revocation on entry only, so a reservation can complete after a
  revoke or expiry; a retried grant enrollment is recognised only when the body is byte-identical,
  so a hostname change between attempts conflicts forever. Both are known and open (#58); a change
  nearby must not worsen them. (#53)
- `already_present` on the pre-scrub hash alone keeps ciphertext scrubbed under old rules or sealed
  to old recipients after a shipper's `state reset`; `shipped-hash` fixes the scrub half, recipient
  rotation is open as #77. (#74, #79)
- A shipper enrolls with its `os.Hostname()`, while an MDM export carries the device manager's
  name. Inventory matching uses the first DNS label, case-folded, and never guesses on a tie;
  re-enrolled machines are always ambiguous because the revoked record keeps the hostname. (#65)

## Build, tooling and UI

- `go-licenses` must run with the toolchain pinned and the workspace off, or it fails with a loader
  error that looks nothing like a license problem; `govulncheck` likewise runs with the workspace
  off to grade the module's own `go` directive. A `go.mod` behind the workspace's Go version hides
  standard-library advisories, and a Dockerfile base image on the older version ships them.
- A dependency bump regenerates `third_party/` and `licenses.csv`, or `make -C fleet-manager check`
  fails on a stale inventory. (#55)
- `go install .../fleet-manager/src@ref` produces a binary named `src`.
- `http.FileServer` asks the host's mime table, which differs per machine; with
  `X-Content-Type-Options: nosniff` a wrong type is a blank admin UI. Content types are pinned in
  `ui.go` and asserted in `ui_test.go`.
- The admin UI once listed every object under `v1/organization=` to find organizations, so login
  time grew with the archive; delimiter listings with bounded concurrency fixed it. (#67)
- A `.github/` directory inside a subdirectory of the repository is inert, and its templates clash
  with the repository's own on import.
