---
name: review-fleet-manager
description: Review a Fleet Manager change (fleet-manager/, its Terraform templates, deploy script, admin UI, operator docs and the fleet-manager workflows) the way this repository's maintainers review - who may do what, what each grant discloses, stored-record and protocol compatibility, vendor defaults, operator docs that are safe to paste, publish and pin discipline. Use for /review-fleet-manager <PR | branch | path>, or when asked to review control-plane code before a PR is marked ready.
argument-hint: "[PR number | branch | path]"
allowed-tools:
  - Read
  - Grep
  - Glob
  - Bash(git diff:*)
  - Bash(git log:*)
  - Bash(git show:*)
  - Bash(git merge-base:*)
  - Bash(git status:*)
  - Bash(gh pr view:*)
  - Bash(gh pr diff:*)
  - Bash(gh pr checks:*)
  - Bash(gh issue view:*)
  - Bash(go test:*)
  - Bash(go vet:*)
---

# Review a Fleet Manager change

Fleet Manager is the control plane a fleet of shippers enrolls against. It issues identities,
serves each organization's collection configuration and age recipients, vends presigned upload
tickets, keeps install names and inventory metadata, and is deployed by operators from the
Terraform templates it ships with. It never receives a trajectory. Everything it must remember is
an object in the bucket, so any replica can die at any moment. A fleet of enrolled shippers cannot
be rolled back the way a stored record can, which is why compatibility and grants get the attention
here. Review for what a maintainer would want fixed before merge, not for a redesign. The design
authority is `CONSTITUTION.md`, the module's invariants are `fleet-manager/ARCHITECTURE.md`, the
threat model is the Scope section of `fleet-manager/SECURITY.md`, and the gate is
`make -C fleet-manager check`.

## 1. Resolve the target

`$ARGUMENTS` is one of:

- A number: a pull request. `gh pr view <n> --json title,body,headRefOid,baseRefName,isDraft,files`
  and `gh pr diff <n>`. The base is `origin/<baseRefName>`.
- A branch name: `git diff $(git merge-base origin/main <branch>) <branch>`.
- A path: that path's diff against `origin/main`, plus uncommitted changes under it.
- Nothing: the commits ahead of the upstream plus uncommitted changes (`git log @{u}..`,
  `git diff @{u}`).

Record the head SHA you review; the summary names it.

Fleet Manager territory is `fleet-manager/`, `.github/workflows/fleet-manager-*.yml` and the root
documents where they describe the control plane. When most changed files are under `src/`, stop
and say the diff belongs to `/review-shipper`. A PR that spans both components gets both skills,
each on its own files, plus one check that both sides read the same wire contract (pass 3).

## 2. Before reviewing

Read, in this order: `fleet-manager/AGENTS.md`, then the root `AGENTS.md` it defers to;
`fleet-manager/ARCHITECTURE.md`; the Scope section of `fleet-manager/SECURITY.md`;
`CONSTITUTION.md`; `fleet-manager/OPERATIONS.md` when the diff touches deployment, records or
telemetry; `.github/PULL_REQUEST_TEMPLATE.md`. Then the PR description and every comment already
on it: `gh pr view <n> --comments`, and the inline threads with
`gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/comments --paginate`. A finding someone already
raised is not raised again. If it is still open at this head, leave it; if the new commits fixed
it, say so in the summary.

For every file the diff touches, read the base version with `git show origin/main:<path>` and
enough of the surrounding code to judge the change. The service is one package under
`fleet-manager/src/`, so follow a route from `server.go` or `admin_http.go` through `manager.go`
to `store.go` rather than judging a handler on its own. A bug that was already there is
pre-existing: list it once at the end, never as a finding.

Load [checklist.md](checklist.md) for the complete checks per area. Load [gotchas.md](gotchas.md)
when the diff touches `terraform/`, a provider file, `deploy.sh`, the Dockerfile, a stored record
or the admin UI; it holds the cloud and tooling facts reviewers had to supply before.

## 3. What counts as a finding

Only a problem this PR introduces, makes worse or newly exposes. Before reporting one:

1. Confirm the PR introduced or exposed it, against the base version.
2. Name a realistic path: which credential, which route, which object, which replica, which
   operator command.
3. Name the concrete consequence: what is read, written, granted, served, deployed or shown that
   should not be, or stops being.
4. Point at the line.
5. Give the fix or its direction when it is short.

If you are not sure it is a bug, it is not a finding. A few findings you are sure of beat many you
are not. When the claim is about behaviour, reproduce it: run `go test ./...` from
`fleet-manager/src` against the in-memory store, write a throwaway test, or trace the Terraform
statement to the key grammar in `store.go`. The summary says what you ran and what you did not;
nobody can apply a template during review, so say so when a grant change is reasoned rather than
observed.

## 4. Review passes, heaviest first

Review attention here has gone to changes in who may do what, what a default customer gets, what a
grant discloses, and what an operator will paste. Do every pass whose area the diff touches.

### Pass 1: who may do what

The reflex P1. Read the permission, the route and the record together.

- Every route added or changed under `/v1/admin` is wrapped in `scopedAdmin` unless a reporter is
  meant to reach it. `adminAuth` admits a reporter credential (`fmr1.*`) to the single health
  report route and nothing else; a reporter that can read or write configuration can take custody
  of the recipients. Decide at the choke point, not per route.
- Device routes (`/v1/config`, `/v2/uploads/authorize`, `/v1/telemetry`) stay behind `deviceAuth`;
  an install reaches its own organization's records and its own prefix, never another's.
- An invite is single-use; a revoked install is served no configuration and no tickets. Known gaps
  are not worsened: completion checks revocation on entry only, and a retried grant enrollment is
  recognised only when the body is byte-identical (#58).
- The served document changes only fields the shipper-protocol rulebook lets the control plane set.
  The recipient list holds what the organization set plus the Quesma recipient only while
  `allow_quesma_etl` is on; `FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL` decides the default for a new
  organization and nothing else.
- A ticket names one key inside the requesting install's own prefix; `s3TicketHeaders` in `aws.go`
  refuses any signed header beyond tagging and metadata; the lifetime is what was asked for.

### Pass 2: grants and what they disclose

The templates are where the least-privilege argument is made; `src/terraform_test.go` pins it.

- Diff `terraform/aws` and `terraform/gcp` statement by statement: each Sid or binding, its
  resource, its prefix or condition. The runtime identity reads nothing below `install=` beyond
  the documented exceptions: `tags.json`, and object metadata for the dedup probe, which S3 and GCS
  authorize as a full read. An external reader is bounded to install objects and
  `control/config.json`.
- A `terraform_test.go` assertion that moves from forbidding to requiring, or a test renamed to
  drop a word like "WriteOnly", is a widening. `fleet-manager/AGENTS.md` makes that a change that
  needs a human's explicit yes: the description states the trade-off and the Constitution article
  it keeps, and the review asks for the yes rather than giving it (#74).
- Content added to a readable object is a widening of the grant that reads it. `tags.json` carries
  administrator-entered personal data; anyone with the read, Quesma included where granted, reads
  it, and the ETL copies it verbatim. `ARCHITECTURE.md` and the README say so (#65).
- A new read of a maybe-absent key on S3 needs `s3:ListBucket` with an `s3:prefix` condition shaped
  like that key, or absence surfaces as 403 and a generic 500; the `InstallAbsence` statements are
  the model. GCS not-found is matched with `errors.Is` against `storage.ErrObjectNotExist`, never
  with `==` (#74).
- What one template grants or describes, the other grants or describes too, or the description
  says why not. A variable description must not name a variable that exists only in the other
  module.

### Pass 3: stored records, protocol and compatibility

A fleet cannot be rolled back. `fleet-manager/AGENTS.md` asks for caution with protocol, object-key,
stored-record and configuration changes, and for a human to confirm any diff in
`src/protocol_test.go`, the wire fixtures or `src/terraform_test.go`.

- A field added to a record (`tags.json`, the organization configuration, the install record, the
  telemetry settings) is rejected or dropped by an older replica, which decodes strictly. The
  description states the replica upgrade order and the rollback path, and names the consumers of
  any list-endpoint shape change (#65).
- Writes follow the classification in `ARCHITECTURE.md`: control records are created with
  `If-None-Match: *` and replaced with `If-Match`; disposable records such as `seen/` are
  unconditional, tagged ephemeral, and never fail a request; nothing is written below `install=`
  except `tags.json`; there is no delete path and no empty object.
- A bucket is versioned, so "replaced" means a new version: a secret written by mistake lives on in
  the old one, and the docs say so (#79).
- A change to `probeStored` or the upload path says what `state reset` on a shipper does after a
  scrub-rule or recipient change; `already_present` on the pre-scrub hash alone keeps stale
  ciphertext (#77 is open) (#74, #79).
- A new shipper-facing behaviour starts in shipper-protocol and arrives as a module bump plus a
  handler. A diff under `src/protocol_test.go` or in a fixture is a claim that the contract should
  change: read it and ask for confirmation.

### Pass 4: defaults and posture

- The Quesma recipient is on by default with an explicit opt-out, because sealing cannot be added
  to an upload after the fact. Telemetry forwarding is off unless a collector is named, because it
  sends something out of the deployment. `defaults_test.go` and
  `TestTerraformOrganizationDefaults` pin both; Quesma's own deployments set the values explicitly
  so a module default cannot silently change production.
- The telemetry signing seed stays under `private/`, outside `v1/`, so a reader granted `v1/*`
  cannot see it; nothing copies it into an archive mirror.
- Logging an install id is fine. A token, a presigned URL, an invite secret or a bucket listing in
  a log or an error is a finding; `TestProbeFailureAuthorizesAsNewAndScrubsTheLog` shows the
  expected shape.

### Pass 5: operator docs and copy-paste safety

The largest theme by count, and the one maintainers fix themselves when the author is slow.

- Enrollment instructions show the command the admin UI shows,
  `quesma-shipper login --server <url> <token>`, and point at per-platform installation, never a
  Linux-only pipe (#52).
- A placeholder that would grant access, such as a role ARN or a bucket name, is a shell variable
  that fails when unset (`${VAR:?...}`), never an example value that works when pasted (#52).
- Numeric or vendor claims link to the page that states them; "Before you begin" lists tools only;
  steps are not repeated across sections; no documentation for an unshipped feature (#52).
- Wherever a document tells someone to run `age-keygen`, it says that recipients encrypt, only the
  private identity decrypts, and the identity is backed up and never committed (#78).
- Every statement the PR makes stale is fixed in the same PR: a code comment, `OPERATIONS.md`, the
  other cloud's `variables.tf`, the shipper-side schema note (#79).

### Pass 6: publishing, pins and deployment

- Both templates default to the `:latest` image, so pushing it is a customer deploy. The publishing
  workflow declares the `fleet-manager-publishing` environment, the Docker Hub secrets live there,
  `fleet-manager-check` stays a required status check, and only one publisher moves `latest` (#52).
- A PR that pins an image names a tag that exists on Docker Hub and contains the change; the
  comment beside the tag stays true; the AWS module's `data "http"` postcondition on
  `/v1/telemetry/public-key` is satisfied by that image; the description lists the checks to run
  after the apply.
- `deploy.sh` and the Makefile refuse a dirty tree, stop on a registry lookup failure rather than
  rebuilding over an existing tag, refuse the default bucket name against a real account, and warn
  that destroying leaves nothing behind only when that is true (#78).
- A Terraform-only change still needs a deploy step and a sentence about what the plan should
  touch.

### Pass 7: hygiene and tooling

- A `go.mod` change regenerates `third_party/` with `make -C fleet-manager licenses` and the
  description argues any new dependency (#55).
- The `go` directive in `go.mod`, the Dockerfile base image and the workspace agree;
  `make -C fleet-manager vulncheck` is clean.
- Nothing under `fleet-manager/` duplicates a root file; workflow files are prefixed
  `fleet-manager-`; no plans in the module.
- Admin UI: a new embedded asset type is pinned in `contentTypes` in `ui.go` and asserted in
  `ui_test.go`, the CSP stays `default-src 'none'`, no third-party request, the credential travels
  only in the `Authorization` header and lives only in tab-scoped session storage.
- No listing of `v1/organization=` without a delimiter; bounded concurrency on fan-out; retries
  against the latest record for administrator edits (#65, #67).

### Pass 8: the PR itself

- Before and after, concretely: a request and its response, a table of cases, the operator's
  command count before and after.
- Compatibility answered for each surface: wire protocol, stored record, object-key grammar,
  served-configuration authority. Permissions answered: does any grant widen, does
  `src/terraform_test.go` still pin what matters. Security effect stated.
- After-merge checks when production runs it; an honest Verified and Not verified list, including
  "the GCP path is not tested" when that is the case; "deliberately not in this PR"; an issue filed
  for each deferred correctness item; "review commit by commit" when a commit is a verbatim copy.

## 5. Do not report

Admin UI styling and JavaScript structure, which merge on screenshots; formatting, vet and lint
that `make -C fleet-manager check` enforces; naming unless it misleads; dependency bumps once
`third_party/` is regenerated; performance beyond listing growth; test counts; GCP or Azure parity
in practice, where stating the gap is enough; edge cases with no realistic path; code the PR does
not touch; refactors the PR does not need. No praise, no restating the diff. Polish goes under
Notes, not as a finding.

## 6. Priorities

- **P1**, fix before merge: a wrong, reporter or revoked credential reaching an administrative
  operation, configuration or tickets; a credential or install reaching another organization; a
  ticket outside the install's own prefix or with an extra signed header; the runtime identity able
  to read sealed payloads, or a reader able to read control records, without the documented
  trade-off and a human's yes; a served document outside the rulebook, or a recipient the
  organization did not set; a stored-record or protocol change an older replica rejects with no
  upgrade order; `latest` published without a gate; a weakened `terraform_test.go` or
  `protocol_test.go`; a secret outside `private/` or in a log.
- **P2**, should fix before merge: a real bug with a narrow trigger, including a 403-as-404 or a
  mis-mapped provider error; a statement the PR made false; a missing compatibility or permissions
  answer; a pin whose tag does not exist or does not contain the change; an operator document that
  misleads about live behaviour; a missing behavioural test at the boundary a reproduction found.
- **P3**, worth a look: a concrete minor issue, a simplification with a concrete gain, a comment
  the change made wrong. At most five; drop the weakest.

## 7. Report

Open with `Reviewed <sha>` and the counts per priority. Then one line per P1 and P2:
`[P1] path:line - what is wrong, when it happens, what it breaks, the fix if short`. Then:

- **Description check**: before-and-after example, compatibility answers, permissions answer with
  the test named, what was not run, scope matches title. One line each, present or missing.
- **Checked and holds**: at most five mechanisms you verified and how, by test run, trace or base
  comparison. Coverage, not praise.
- **Needs a human's yes**: any grant, contract or default change the description flags or that you
  found, stated as the question a maintainer must answer.
- **Notes**: P3s and polish, at most five.
- **Pre-existing**: anything you found that the PR did not introduce, one line each, so it can be
  filed.

Post nothing to GitHub unless asked. When asked to comment, post each finding inline on its line
with the `[P1]`, `[P2]` or `[P3]` prefix and the summary as one PR comment. Never approve or
request changes on a maintainer's behalf.
