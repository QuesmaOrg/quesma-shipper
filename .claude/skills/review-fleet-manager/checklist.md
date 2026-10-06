# Fleet Manager review checklist

Every check here was asked for in a real review of the control plane or is pinned by a document a
reviewer cited. Weight: **H** has blocked or reversed a merge, or recurs; **M** was asked for and
fixed before merge; **L** came up once. Numbers are quesma-shipper pull requests or issues where
the check was learned; checks learned before the module moved into this repository carry no
number. "Codified" means `fleet-manager/AGENTS.md`, `fleet-manager/ARCHITECTURE.md`,
`fleet-manager/SECURITY.md`, `CONSTITUTION.md` or the PR template already states the rule; the
rest is known only from review history.

Open the areas the changed paths map to, run each check against the diff and the base version,
and report only what the PR introduced or made worse.

| Changed path | Areas |
| --- | --- |
| `src/server.go`, `src/admin_http.go`, `src/manager.go`, `src/model.go`, `src/health.go` | A, C, K |
| `src/upload.go`, `src/aws.go`, `src/gcp.go`, `src/azure.go` | B, D, E |
| `src/store.go`, `src/seen.go`, `src/tags.go`, `src/metadata.go` | D, J |
| `src/telemetry*.go` | A, G |
| `src/main.go`, `src/defaults_test.go` | C |
| `src/ui.go`, `src/admin-ui/`, `tests/` | H |
| `terraform/` | E, F, I |
| `deploy.sh`, `Dockerfile`, `Makefile`, `go.mod`, `third_party/`, `.github/workflows/fleet-manager-*` | F, M |
| `src/protocol_test.go`, `src/terraform_test.go`, fixtures | J, K |
| `README.md`, `OPERATIONS.md`, `WALKTHROUGH.md`, `ARCHITECTURE.md`, `SECURITY.md`, `terraform/*/README.md` | I |
| any renamed or redefined term | I.48 |

## A. Authentication and authorization

1. **Admin route scope.** Every route under `/v1/admin` is wrapped in `scopedAdmin`, or in
   `adminOnly` for the organization list, create, defaults and configuration routes. Bare `scoped`
   admits a reporter credential (`fmr1.*`); only the health report route uses it, and
   `reporterRoute` plus `health_test.go` pin that. A reporter that can read configuration and its
   ETag can also replace it. [H, codified in SECURITY.md]
2. **Decide at the choke point.** Credential kind is known in `adminAuth`; anything added later is
   administrative by default. A per-route check is a finding waiting to be forgotten. [H]
3. **Device routes stay behind `deviceAuth`.** `/v1/config`, `/v2/uploads/authorize` and
   `/v1/telemetry` resolve the organization from the install's own credential, never from the
   request body. [H, codified]
4. **Invites and revocation.** An invite enrolls once. A revoked install is served no configuration
   and no tickets, and the revoked record stays, so a re-enrolled machine is a separate identity;
   the UI says so (#65). Known gaps are not worsened: revocation is checked on entry to enrollment
   only, and a retried grant enrollment matches only a byte-identical body (#58 is open). [M; #53,
   #65]
5. **Organization isolation.** A credential or install scoped to one organization reaches no other
   organization's control records, configuration or objects; every key the handler builds carries
   the organization and install from the credential. [H, codified]
6. **Admin credential handling.** The credential travels only in the `Authorization` header and
   lives only in tab-scoped session storage in the UI; it is never logged or placed in a URL.
   [M, codified]

## B. Upload tickets and the dedup probe

7. **One key, own prefix.** A ticket names exactly one key inside the requesting install's prefix;
   the key is validated against the grammar, not just prefixed. [H, codified]
8. **Closed header contract.** `s3TicketHeaders` in `aws.go` admits tagging and `x-amz-meta-*`
   only and refuses anything else rather than passing it through; the lifetime is what was asked
   for. [H, codified]
9. **The probe trusts the right hash.** For a change to `probeStored` or the upload path, the
   description states what a shipper's `state reset` does after a scrub-rule change, after a
   recipient change, and during a rolling upgrade with old and new replicas answering side by
   side; `already_present` on the pre-scrub hash alone keeps stale ciphertext (#77 is open).
   [M; #74, #79]
10. **Probe failures authorize as new and scrub the log.** A storage error during the probe never
    blocks an upload and never prints the key list; `TestProbeFailureAuthorizesAsNewAndScrubsTheLog`
    pins the shape. [M; #74]
11. **Absence is 403 on S3.** A read of a maybe-absent key needs `s3:ListBucket` with an
    `s3:prefix` condition shaped like the key, as the `InstallAbsence` statements do; otherwise the
    first write path fails with a generic error. GCS returns 404 and needs no such grant. [M; #74]
12. **Provider errors are matched with `errors.Is`.** GCS wraps not-found, so `==` and type
    assertions miss it and every absent object surfaces as an error. A 403 stays an error. [M; #74]

## C. Served configuration and recipients

13. **Authority rulebook.** The served document changes only the fields the shipper-protocol
    rulebook lets the control plane set; a new field arrives through a protocol release and a
    module bump. [H, codified]
14. **Recipients are what the organization set.** The Quesma recipient appears only while
    `allow_quesma_etl` is on; `FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL` decides the default for a new
    organization and nothing else; disabling affects future uploads only, and the docs say so.
    [H, codified]
15. **Defaults are argued.** Quesma recipient on by default with opt-out, because sealing cannot be
    added after the fact; telemetry forwarding off unless a collector is named. `defaults_test.go`
    and `TestTerraformOrganizationDefaults` pin them; Quesma's own deployments set values
    explicitly. [H]
16. **Configuration edits are conditional.** A PUT carries the ETag it read; a lost race is a
    conflict, never a silent overwrite. [M, codified]

## D. Storage layer and records

17. **One durable-state primitive.** Anything that must survive a restart is an object in the
    bucket; no cache, no local disk, no database, no lock service. [H, codified]
18. **Control records are conditional.** Created with `If-None-Match: *`, replaced with `If-Match`;
    replicas coordinate through the store and nowhere else; an organization is never created in a
    bucket without versioning. [H, codified]
19. **Disposable records never fail a request.** `seen/` uses unconditional writes, is tagged
    `lifecycle=ephemeral`, collapses check-ins within `seenWriteInterval`, and a failed write is
    logged and dropped. The hot path never rewrites a security record. [H, codified]
20. **Nothing below `install=` except `tags.json`.** It sits in the install's own root so a bucket
    walker resolves an id without the service; its writes are conditional and retried against the
    latest record. [H, codified]
21. **No delete, no empty object.** No route deletes; no organization is removed; a GCS
    `storage.objects.delete` exists only because replacing bytes there needs delete-plus-create,
    and versioning keeps what is displaced. [H, codified]
22. **Strict decoders in older replicas.** A new record field is dropped or rejected by a replica
    still running the old binary; the description states the replica upgrade order and the rollback
    path, and names the consumers of any list-endpoint shape change. [H, codified; #65]
23. **Versioned bucket semantics in prose.** "Replaced" is a new version; a secret written by
    mistake lives on in the old one. [M; #79]
24. **Readable content is a grant.** Adding personal data to `tags.json` widens what the existing
    read discloses to everyone holding it, Quesma included where granted; `ARCHITECTURE.md` and
    the README say who reads it. [H, codified since #65]

## E. Terraform templates and grants

25. **Statement by statement.** For each Sid or binding changed: resource, prefix, condition. The
    runtime identity reads nothing below `install=` beyond `tags.json` and the object metadata the
    probe needs, which S3 and GCS authorize as a full read; the description names that trade-off
    where it applies. [H, codified; #74]
26. **External readers are bounded** to install objects and `control/config.json`, current
    versions only. [H]
27. **Test direction.** A `terraform_test.go` assertion that moves from forbidding to requiring,
    or a test renamed to drop "WriteOnly" or similar, is a widening: the description states the
    trade-off and the Constitution article kept, and the review asks for a human's explicit yes.
    Run the new test against the base tree, unpacked with `git archive`, to confirm it fails
    there. [H, codified; #74]
28. **Parity in words and grants.** A statement added to one template has its counterpart in the
    other or an explanation; a variable description never names a variable that exists only in
    the other module. [M]
29. **Lock files stay committed.** `.terraform.lock.hcl` files are tracked on purpose. [L]
30. **A Terraform-only change still deploys.** The description says what the plan should touch and
    nothing else. [M]

## F. Deployment, publishing, pins, tooling

31. **`latest` is a customer deploy.** Both templates default to it. The publishing workflow
    declares the `fleet-manager-publishing` environment and the Docker Hub secrets live in it;
    `fleet-manager-check` is a required status check; exactly one publisher moves `latest`. [H; #52]
32. **Pin PRs.** The tag's publishing run succeeded (`gh run list -w fleet-manager-image.yml`) and
    contains the change, or the PR waits; the comment
    beside it stays true about which repository's commit it names; the `data "http"` postcondition
    on `/v1/telemetry/public-key` is satisfied by that image; post-apply checks are listed. [M]
33. **`deploy.sh` and Makefile safety.** Dirty tree refused; a registry lookup failure stops the
    deploy instead of rebuilding over an existing tag; the default bucket name is refused against
    a real account; destroy paths say what remains; a dev target falls back to the built binary
    when offline. [M; #78]
34. **Toolchain alignment.** The `go` directive, the Dockerfile base image and the workspace agree;
    the `govulncheck` job in `gh pr checks` is green. [M]
35. **Dependencies.** A `go.mod` change regenerates `third_party/` with `make -C fleet-manager
    licenses`; a new dependency is argued in the description. [M, codified; #55]
36. **Docker context.** `.dockerignore` patterns are recursive where they must be; no local
    environment file reaches an image. [L]

## G. Telemetry proxy and identity

37. **Forwarding is admission-gated.** Only to the collector the organization configured; the
    collector's response body is never relayed; a deployment with forwarding off still reads and
    writes its signing identity at startup. [M, codified]
38. **The seed is a key.** `private/fleet-manager/telemetry-identity.json` lives outside `v1/` so a
    `v1/*` reader cannot see it; no archive mirror copies `private/`. [H, codified]

## H. Admin UI

39. **Asset types are pinned.** A new embedded asset type is in `contentTypes` in `ui.go` and
    asserted in `ui_test.go`, because the host mime table differs and `nosniff` turns a wrong type
    into a blank page. [L]
40. **CSP stays `default-src 'none'`**, no third-party requests, no inline secrets. [M, codified]
41. **Listing cost.** Finding organizations uses delimiter listings and bounded concurrency; login
    time must not grow with the archive (#67). [M]
42. **Screenshots are enough for styling.** Do not review CSS or JavaScript structure; do review
    what the UI claims, such as the enrollment command and the revocation wording. [L]

## I. Operator documentation

43. **Commands are what the UI shows.** `quesma-shipper login --server <url> <token>`; per-platform
    installation, never a Linux-only pipe. [M; #52]
44. **Placeholders fail closed.** `${VAR:?message}` for anything that grants access; never a literal
    example ARN or bucket. [H; #52]
45. **Claims are sourced; sections do not repeat.** Numeric requirements link to the vendor page;
    "Before you begin" lists tools only; one canonical procedure. [M; #52, #72]
46. **No docs for unshipped features.** [M; #52]
47. **Age custody explained at every `age-keygen`.** Recipients encrypt, only the private identity
    decrypts, back it up, never commit it; show the command once with its output. [M; #78]
48. **Stale statements fixed here.** For every term the PR renames, redefines or removes, grep the
    head tree outside tests (`git grep -n -i -e '<term>' <head> -- . ':!*_test.go'`) and the
    shipper-protocol module in the module cache; each hit is updated in this PR or named in the
    description with a reason. Past misses sat in code comments, `OPERATIONS.md`, the other
    cloud's `variables.tf` and a shipper-side schema note. Not in a follow-up. [M; #79]
49. **Guarantee wording.** The service cannot decrypt, but its identity can download ciphertext
    where the probe grant applies; `tags.json` is readable; an install id does not identify a
    person, while sealed content may. Say exactly that. [M]

## J. Protocol and compatibility

50. **Contract changes start upstream.** A new shipper-facing behaviour is a shipper-protocol
    release, then a module bump plus a handler here. [H, codified]
51. **A diff in `protocol_test.go` or a fixture is a claim.** Read it, explain it in the PR, ask for
    confirmation. [H, codified]
52. **Object-key grammar.** A new key shape or `source=` value is a protocol change with ETL
    consequences; name them. [H, codified]
53. **Both sides read the same contract** when a PR spans `src/` and `fleet-manager/`. [M]

## K. Tests

54. **Behavioural tests at the boundary found.** Ask for the test that fails on the old code at the
    exact scenario, such as a reset after a recipient change; never for coverage. [M; #74]
55. **In-memory store first.** Every non-provider file is testable against the in-memory store;
    a test that needs a cloud account is tagged live and is not the only cover. [M, codified]
56. **Pinned tests stay strict.** `terraform_test.go`, `protocol_test.go`, `defaults_test.go`,
    `ui_test.go`; a weakening is a human decision. [H, codified]
57. **Removal is covered elsewhere.** A deleted test is named with the end-to-end test that still
    covers it. [L; #60]

## L. Description

58. **Before and after**, concretely. [M, codified]
59. **Compatibility for each surface**: wire protocol, stored record, object-key grammar,
    served-configuration authority. [M, codified]
60. **Permissions**: does any grant widen, does `src/terraform_test.go` still pin what matters, what
    may the runtime identity or a reader now do. [H, codified]
61. **Security effect stated**, or "none". [M, codified]
62. **After-merge checks** when production runs it; **Verified and Not verified** honestly, including
    untested cloud paths; **deliberately not in this PR**; an issue per deferred correctness item;
    "review commit by commit" when a commit is a verbatim copy. [M; #52, #74, #79]

## M. Repository hygiene

63. **No duplicate root files under `fleet-manager/`**: no second code of conduct, contributing
    guide, trademark notice or `.github/`; workflows are prefixed `fleet-manager-`; no plans in
    the module. [L]
64. **Synthetic test data only**: no real control records, logs with real identifiers or bucket
    listings. [M, codified]
