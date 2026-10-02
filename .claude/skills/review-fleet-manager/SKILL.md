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
  - Bash(git grep:*)
  - Bash(git merge-base:*)
  - Bash(git status:*)
  - Bash(git cat-file:*)
  - Bash(git fetch origin pull/:*)
  - Bash(git archive:*)
  - Bash(tar -x:*)
  - Bash(mktemp:*)
  - Bash(gh pr view:*)
  - Bash(gh pr diff:*)
  - Bash(gh pr checks:*)
  - Bash(gh issue view:*)
  - Bash(gh run list:*)
  - Bash(gh api repos/QuesmaOrg/quesma-shipper/pulls:*)
  - Bash(go env:*)
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

- A number: a pull request.
  `gh pr view <n> --json title,body,state,mergedAt,isDraft,headRefOid,baseRefOid,baseRefName,files`
  and `gh pr diff <n>`. The base is `baseRefOid`, not `origin/<baseRefName>`, which may have moved
  on. Record `state`: for a merged PR the findings are issues to file, not change requests.
- A branch name: `git diff $(git merge-base origin/main <branch>) <branch>`; the base is that
  merge-base.
- A path: that path's diff against `origin/main`, plus uncommitted changes under it.
- Nothing: the commits ahead of the upstream plus uncommitted changes (`git log @{u}..`,
  `git diff @{u}`).

Record the head SHA you review; the summary names it. Line numbers in findings are head line
numbers as `gh pr diff` shows them on the `+` side; a removed line is cited with its base number
and marked `(base)`.

Fleet Manager territory is `fleet-manager/`, `.github/workflows/fleet-manager-*.yml` and the root
documents where they describe the control plane. Decide ownership by where the behaviour changes,
not by counting files: when the behaviour lives in `src/` and the `fleet-manager/` files only
follow it, stop and say the diff belongs to `/review-shipper`. When behaviour changes on both
sides, review the `fleet-manager/` files here, list the `src/` files under "Handed to
/review-shipper" in the report, keep the cross-contract check from pass 3 in this report, and then
run `/review-shipper <target>` as its own review.

**Short path.** When the diff changes no route, grant, template, stored-record field, image pin,
default or protocol fixture, say so, read only the `ARCHITECTURE.md` paragraph the diff touches,
and run passes 3, 5 and 8.

## 2. Before reviewing

Read once per session: `fleet-manager/AGENTS.md` and the root `AGENTS.md` it defers to;
`fleet-manager/ARCHITECTURE.md`; the Scope section of `fleet-manager/SECURITY.md`;
`.github/PULL_REQUEST_TEMPLATE.md`. Later in the session, grep them for the terms in the diff and
reread only those paragraphs. Read `CONSTITUTION.md` and `fleet-manager/OPERATIONS.md` when the
diff touches a record, a grant, deployment or telemetry.

Read the PR description and everything already said on it:

```sh
gh pr view <n> --comments
gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/reviews --paginate --jq '.[] | "\(.commit_id[0:7]) \(.state) \(.user.login): \(.body)"'
gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/comments --paginate --jq '.[] | "\(.path):\(.line // .original_line) @\(.original_commit_id[0:7]) \(.user.login): \(.body)"'
```

`original_commit_id` says which commit a thread was raised on, which decides whether a later commit
answered it. A finding someone already raised is not raised again: if it is still open at this
head, name it in one line as open from the earlier review; if a later commit fixed it, say so in
one line.

For every file the diff touches, read the base version with `git show <base>:<path>` and enough of
the surrounding code to judge the change. The service is one package under `fleet-manager/src/`,
so follow a route from `server.go` or `admin_http.go` through `manager.go` to `store.go` rather
than judging a handler alone. A bug that was already there is pre-existing: list it once at the
end, never as a finding.

Load [checklist.md](checklist.md); its first table maps changed paths to the areas to open. Load
[gotchas.md](gotchas.md); it holds the cloud, record and tooling facts reviewers had to supply
before, and it is short enough to read every time.

The wire contract is the shipper-protocol module at the version in `fleet-manager/go.mod`, readable
offline at `$(go env GOMODCACHE)/github.com/!quesma!org/shipper-protocol@<version>`: `PROTOCOL.md`
for the prose, `authority.json` for which served fields the control plane may set, `schemas/` and
`schemas/v2/` for the message shapes, `fixtures/` for the pinned bytes.

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
are not.

Tests and reproductions run on the head you review, never on whatever the working tree has
checked out. `git cat-file -t <headRefOid>` tells you whether the commit is local; if not,
`git fetch origin pull/<n>/head`. Then unpack it beside the repository and run from there:

```sh
dir=$(mktemp -d) && git archive <headRefOid> | tar -x -C "$dir" && (cd "$dir/fleet-manager/src" && go test ./...)
```

Run the new tests against the base the same way when a claim is "this fails on main". Some checks
cannot be made from a checkout: whether a Docker Hub tag exists is answered by
`gh run list -w fleet-manager-image.yml --json headSha,conclusion`, the vulnerability scan by the
`govulncheck` job in `gh pr checks <n>`, and a live cloud behaviour only by the author. Say which
of these you used, and say when a grant change was reasoned about rather than observed.

## 4. Review passes, heaviest first

Heaviest means consequence, not frequency: passes 1 and 2 hold the findings that reverse a merge,
pass 5 holds the most frequent ones. Do every pass whose area the diff touches and name the ones you
skipped.

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
- The served document changes only fields `authority.json` lets the control plane set. The
  recipient list holds what the organization set plus the Quesma recipient only while
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
  `control/config.json`, current versions only.
- When a provider file gains a read, a HEAD, a list or a write and the templates do not change,
  check the new call against the existing statements: the need for a grant can change while the
  grant does not, and the first failure is a generic error in production.
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
  the old one, sealed to the recipients of that time, and the docs say so (#79).
- For a change to `probeStored` or the upload path, the description states what a shipper's
  `state reset` does after a scrub-rule change, after a recipient change, and during a rolling
  upgrade while old and new replicas answer side by side (#74, #79; recipient rotation is open as
  #77).
- A new shipper-facing behaviour starts in shipper-protocol and arrives as a module bump plus a
  handler. In `src/protocol_test.go` or a fixture, a diff that changes an asserted wire shape,
  header, status or fixture bytes is a contract claim: read it and ask for confirmation. A diff
  that only changes which inputs earn an existing answer is behaviour: review it under the probe
  bullet, and still list it under "Needs a human's yes", because `fleet-manager/AGENTS.md` makes
  every diff there one. When the PR spans both components, check that `src/` and `fleet-manager/`
  read the same contract version and the same field names.

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

### Pass 5: operator docs and statements the PR made stale

The most frequent finding, and the one maintainers fix themselves when the author is slow.

- Enrollment instructions show the command the admin UI shows,
  `quesma-shipper login --server <url> <token>`, and point at per-platform installation, never a
  Linux-only pipe (#52).
- A placeholder that would grant access, such as a role ARN or a bucket name, is a shell variable
  that fails when unset (`${VAR:?...}`), never an example value that works when pasted (#52).
- Numeric or vendor claims link to the page that states them; "Before you begin" lists tools only;
  steps are not repeated across sections; no documentation for an unshipped feature (#52).
- Wherever a document tells someone to run `age-keygen`, it says that recipients encrypt, only the
  private identity decrypts, and the identity is backed up and never committed (#78).
- Stale statements: for every term the PR renames, redefines or removes, grep the head tree
  outside tests and the shipper-protocol module in the module cache, and check each hit was updated
  or is named in the description with a reason. Comments, `OPERATIONS.md`, the other cloud's
  `variables.tf` and shipper-side schema notes are where past misses sat (#79):

  ```sh
  git grep -n -i -e '<term>' <headRefOid> -- . ':!*_test.go'
  grep -rn -i -e '<term>' "$(go env GOMODCACHE)/github.com/!quesma!org/shipper-protocol@<version>"
  ```

### Pass 6: publishing, pins and deployment

- Both templates default to the `:latest` image, so pushing it is a customer deploy.
  `fleet-manager-image.yml` publishes on every push to `main` that touches `fleet-manager/`; it
  declares the `fleet-manager-publishing` environment, the Docker Hub secrets live there,
  `fleet-manager-check` stays a required status check, and only one publisher moves `latest` (#52).
- A PR that pins an image names a tag whose publishing run succeeded
  (`gh run list -w fleet-manager-image.yml`) and contains the change; the comment beside the tag
  stays true; the AWS module's `data "http"` postcondition on `/v1/telemetry/public-key` is
  satisfied by that image; the description lists the checks to run after the apply.
- `deploy.sh` and the Makefile refuse a dirty tree, stop on a registry lookup failure rather than
  rebuilding over an existing tag, refuse the default bucket name against a real account, and warn
  that destroying leaves nothing behind only when that is true (#78).
- A Terraform-only change still needs a deploy step and a sentence about what the plan should
  touch.

### Pass 7: hygiene and tooling

- A `go.mod` change regenerates `third_party/` with `make -C fleet-manager licenses` and the
  description argues any new dependency (#55).
- The `go` directive in `go.mod`, the Dockerfile base image and the workspace agree; the
  `govulncheck` job in `gh pr checks` is green.
- Nothing under `fleet-manager/` duplicates a root file; workflow files are prefixed
  `fleet-manager-`; no plans in the module.
- Admin UI: a new embedded asset type is pinned in `contentTypes` in `ui.go` and asserted in
  `ui_test.go`, the CSP stays `default-src 'none'`, no third-party request, the credential travels
  only in the `Authorization` header and lives only in tab-scoped session storage.
- No listing of `v1/organization=` without a delimiter; bounded concurrency on fan-out; retries
  against the latest record for administrator edits (#65, #67).

### Pass 8: the PR itself

Check the description against the list in section 7 and report what is missing. The two items
reviewers most often had to ask for: a concrete before and after, and the honest "Not verified"
line, including "the GCP path is not tested" when that is the case.

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

Open with `Reviewed <sha>` and the counts per priority, or `No P1 or P2 findings.` Then one line
per P1 and P2: `[P1] path:line - what is wrong, when it happens, what it breaks, the fix if short`.
Then:

- **Description check**, one line each, present or missing: before and after; compatibility for
  the wire protocol, stored records, the object-key grammar and served-configuration authority;
  permissions, with `src/terraform_test.go` named; security effect; after-merge checks when the
  change reaches the published image; verified and not verified; deliberately not in this PR;
  issues filed for deferred items; scope matches title.
- **Needs a human's yes**: any grant, contract, default or pinned-test change, stated as the
  question a maintainer must answer.
- **Checked and holds**: at most five mechanisms you verified and how, by test run, trace or base
  comparison. Coverage, not praise.
- **Ran / not run**: tests and reproductions, with the SHA they ran on; passes skipped and why.
- **Handed to /review-shipper**: the `src/` files you left to the other skill, if any.
- **Notes**: P3s and polish, at most five.
- **Pre-existing**: anything you found that the PR did not introduce, one line each, so it can be
  filed.

Post nothing to GitHub unless asked. When asked to comment, post each finding inline on its line
with the `[P1]`, `[P2]` or `[P3]` prefix and the summary as one PR comment. Never approve or
request changes on a maintainer's behalf.
