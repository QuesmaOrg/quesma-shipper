# README review from a newcomer’s perspective

- **Verdict:** The root introduction and diagram already establish the two components and direct uploads. A newcomer can grasp the broad idea, but cannot yet derive a consistent account of who can read/decrypt data, what enrollment means, or how to complete every setup route. Preserve the useful operator/developer split; fix the contradictions and make the reading paths explicit.
- **Review date and snapshot:** 2026-10-02. Reviewed the working checkout whose README contents are now in `0239318ff7909a650de93ed8e3ccfc56c8c3666b`, including the pending Windows managed-install documentation. Line references below describe that snapshot, not necessarily current `main`. The review-only PR is based on `eaef886`; Windows-specific findings are identified below.
- **Plan executed:** Three independent adversarial passes covered architecture/trust, operator setup, and navigation/terminology. Findings were consolidated and checked against the constitution, source, deployment templates, and existing tests. This file records proposed fixes; it does not implement them.
- **Scope:** All ten READMEs: root; `src/`; `fleet-manager/`; AWS and GCP Terraform; Homebrew; macOS MDM; Windows Intune; Windows `README-System.md`; Windows installer tests. Linked walkthrough, operations, architecture, and security documents were checked where they affect those journeys.
- **Priority:** **P1** = misleading confidentiality guarantee, blocked primary setup, or risk of losing deployment state. **P2** = material conceptual, navigation, or operational gap. Editorial recommendations are identified separately from verified contradictions.

## P1: fix before relying on the onboarding instructions

- **R01 — Distinguish access to ciphertext from the ability to decrypt it.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 201–205 versus 296–302; related [security policy](fleet-manager/SECURITY.md), lines 48–58.
  - **Problem:** The README says Fleet Manager has no permission to read trajectory objects, then correctly says its runtime can fetch every sealed payload. The linked policy still treats runtime payload-read permission as a vulnerability. These are incompatible explanations of the central trust boundary.
  - **Fix:** State consistently: shippers upload directly to storage; Fleet Manager’s normal code reads control records, install tags, and object metadata; its cloud identity can download ciphertext; it holds no private age identity capable of decrypting the collected payloads. Explain that metadata probes require the broader cloud read permission. Bring the linked policy into agreement without changing IAM grants.
  - **Evidence:** [AWS template](fleet-manager/terraform/aws/main.tf), lines 173–179; [GCP template](fleet-manager/terraform/gcp/main.tf), lines 95–101 and 129–135; [Fleet Manager architecture](fleet-manager/ARCHITECTURE.md), lines 42–50.

- **R02 — Explain all decryption recipients at the first privacy claim.**
  - **Where:** [Root README](README.md), lines 24–25 and 44–48; [Fleet Manager README](fleet-manager/README.md), lines 189–210.
  - **Problem:** “Only the holders of the organisation’s age keys” and custodians’ identities being “the only way” imply exclusively customer-held decryption keys. Quesma’s public recipient is included by default. Requiring two custodians also leaves unclear whether both keys are needed together.
  - **Fix:** Say that each configured recipient’s private identity can independently decrypt an upload. Name customer custodians and the default Quesma ETL recipient in the introductory explanation. Explain ETL as the separate analytics reader, that it needs bucket access or copies of objects as well as its key, and that disabling its recipient affects future uploads only. State explicitly that two custodians provide independent access/recovery, not two-person cryptographic approval. Keep private identities outside Fleet Manager.
  - **Evidence:** [Configuration rendering](fleet-manager/src/server.go), lines 269–279, appends the Quesma recipient; the fuller access explanation already exists in the Fleet Manager README.

- **R03 — Reconcile the redaction promises with account/usage records that bypass scrubbing.**
  - **Where:** [Root README](README.md), lines 5–6; [client README](src/README.md), lines 59–67, 94–115, and 185–190; [root security policy](SECURITY.md), lines 67–69.
  - **Problem:** “Every file” is described as scrubbed, account/usage records explicitly skip scrubbing, and line 115 then says everything derived from credential stores is scrubbed. The introduction also reads as a guarantee that all secrets and personal data are removed.
  - **Fix:** Describe configured secret/PII detection and redaction for transcript/context files. State the compiled account/usage exception beside the first collection/privacy summary: those records preserve metadata without scrubbing, while all uploaded payloads remain encrypted. Restrict line 115 to enriched conversation content. Qualify the security bullets and avoid promising perfect detection.
  - **Evidence:** Account sources set `scrub: false` in [Claude Code](src/internal/formats/catalogdata/claude-code.yaml), line 103, [Codex](src/internal/formats/catalogdata/codex.yaml), line 63, and [Cursor](src/internal/formats/catalogdata/cursor.yaml), line 75. [Account tests](src/internal/engine/account_test.go), lines 125–154, pin the preservation of these bytes; Constitution Article 4 allows compiled exemptions.

- **R04 — Repair the default deployment command’s missing approval prompt.**
  - **Where:** [Root README](README.md), lines 55–62; [walkthrough](fleet-manager/WALKTHROUGH.md), lines 35–40; [deployment script](fleet-manager/deploy.sh), lines 72 and 105.
  - **Problem:** The docs promise a plan followed by an interactive `yes`. The script passes `-input=false` without `-auto-approve` unless `-y` was supplied. Both tools disable the approval prompt and fail in that mode. This blocks the documented default deployment; ordinary `destroy` has the same mismatch. See [Terraform apply options](https://developer.hashicorp.com/terraform/cli/commands/apply#apply-options) and [OpenTofu apply options](https://opentofu.org/docs/cli/commands/apply/#apply-options).
  - **Fix:** Correct the script so its default apply/destroy path permits interactive approval. Reserve noninteractive approval for explicit `-y`. Preserve the documented opportunity to review the plan; do not solve this by adding unconditional approval to the newcomer examples. Verify both default and explicit noninteractive argument paths without provisioning cloud resources.

- **R05 — Preserve the trial’s Terraform state when cleanup fails.**
  - **Where:** [Walkthrough](fleet-manager/WALKTHROUGH.md), lines 3–6, 19, 27–30, and 158–175.
  - **Problem:** `mkdir -p ~/fm-trial` can reuse an existing directory. The final commands remove that directory even if the preceding Terraform destroy failed. A failed bucket cleanup/destroy can therefore leave cloud resources running after their local state has been deleted. “Nothing remains” is stronger than the demonstrated cleanup.
  - **Fix:** Create a new unique trial directory and retain its path explicitly. Stop on bucket/API/destroy failures. Remove local state only after successful destruction has been verified; otherwise retain it with recovery instructions. Scope cleanup to that trial and state what remains, such as downloaded base images/caches. Do not remove an arbitrary existing `~/fm-trial` directory.
  - **Evidence:** The independent shell commands at lines 170–172 do not condition state deletion on destroy success; [deploy.sh](fleet-manager/deploy.sh), lines 101–105, deliberately refuses a nonempty bucket.

## P2: clarify the product model and the reading paths

- **R06 — Complete the introductory lifecycle and define the different credentials.**
  - **Where:** [Root README](README.md), lines 7–24 and 59–75; [Fleet Manager README](fleet-manager/README.md), lines 3–18. This is a structural omission, not an incorrect claim that STS credentials are currently issued.
  - **Problem:** “Control plane,” “stateless,” “upload tickets,” “grant,” and “custodian” precede a complete plain-language explanation. Enrollment and signing dominate the root description; centrally supplied collection configuration and local progress tracking receive little explanation.
  - **Fix:** Put a short lifecycle beside the diagram: install the `quesma-shipper` binary as a background collector; enroll with the independently deployed Fleet Manager; fetch collection configuration and public recipients; scrub/encrypt locally; obtain a short-lived per-object signed PUT URL; upload directly; record success locally. Say that one Fleet Manager deployment manages many installs and persists its control records in the bucket, which is why replicas can be stateless. Add a separate authorized-reader/custodian node that downloads and decrypts outside Fleet Manager.
  - **Fix the vocabulary:** Distinguish the operator’s cloud deployment credentials, Fleet Manager admin credential, invite/reusable enrollment grant, enrolled device signing credential, temporary upload URL, public age recipient, and private decryption identity. In this implementation, “vends upload authorization” means issuing signed URLs; it does not mean handing the shipper permanent AWS/GCP credentials. Define an organization as the grouping for collection policy, recipients, and enrollment.
  - **Evidence:** [Enrollment record](src/internal/controlplane/enrollment.go), lines 24–33; [upload ticket types](src/internal/controlplane/uploads.go), lines 26–42 and 64–80; [AWS signing](fleet-manager/src/aws.go), lines 159–183; Constitution Articles 1, 2, and 7.

- **R07 — Define an install independently of a physical machine.**
  - **Where:** [Root README](README.md), lines 73–78 and 93–110; [macOS MDM README](src/packaging/macos/mdm/README.md), lines 18–24. Also applies to the pending Windows system guide, lines 3–13.
  - **Problem:** “One invite per developer machine” conflicts with supported all-users packages, which start separately enrolled collectors for each OS user. A single-use invite cannot enroll all users of a shared workstation.
  - **Fix:** Define an install as an enrolled shipper identity with its own local state, normally one OS user on one machine. Use “one invite per user installation” and recommend reusable grants for managed rollouts. Include the concrete example: two developers sharing one computer appear as two installs. Retain the useful “deploy Fleet Manager once / install collectors across the fleet” distinction.

- **R08 — Remove the claim that uploaded content cannot identify its owner.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 215–218; related Name tooltip in [admin UI](fleet-manager/src/admin-ui/index.html), line 80.
  - **Problem:** “Nothing a shipper uploads says who holds the machine” conflates a UUID in an object key with anonymity of the encrypted content. Collected account snapshots can carry account identifiers and email/profile information.
  - **Fix:** Say the install UUID itself does not identify a person, and administrator-managed `tags.json` provides an explicit owner mapping. State that encrypted payloads may contain identifying account metadata. Preserve the later warning that tags themselves are readable to holders of bucket read access.
  - **Evidence:** [Codex account collector](src/internal/sources/accounts_codex.go), lines 28–40, preserves ID-token claims; [account test fixture](src/internal/sources/accounts_test.go), lines 48–49, includes an email claim.

- **R09 — Lead the Fleet Manager README with operator choices, then put contributor/reference material later.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 3–18, 26–85, and 173–332. Editorial recommendation.
  - **Problem:** Directory visitors encounter storage prefixes and “acknowledgement, or cursor path,” then contributor checks and overlapping local-run recipes, before the key/enrollment explanation. Detailed tags APIs and inventory matching dominate the latter half. Readers must repeatedly switch between this file and numbered steps in the root README.
  - **Fix:** Start with responsibilities, persistence, trust boundaries, and cloud/local deployment choices. Give an ordered route through deployment, organization creation, enrollment, and verification. Move contributor commands last and detailed metadata/API/telemetry mechanics into linked reference material. Choose one canonical operator procedure; replace references such as “steps 2, 4 and 5” with named, stable sections. A directory README can summarize and link without duplicating the entire cloud guide.

- **R10 — Expose local evaluation and managed rollout at the initial route chooser.**
  - **Where:** [Root README](README.md), lines 30–36, 86–91, and 112–113. Editorial recommendation.
  - **Problem:** “Try it first” selects a cloud deployment; the no-cloud trial appears only after the cloud setup. MDM/Intune appears after personal installation. Newcomers must read the wrong path before discovering the suitable one.
  - **Fix:** Offer explicit choices up front: evaluate locally without cloud; disposable AWS walkthrough; deploy Fleet Manager in AWS/GCP; join an existing organization personally; deploy through IT/MDM. Mark prerequisites and intended audience for each. Link directly to the relevant guide, with a clear next step after completion.

## P2: make setup and operation reproducible

- **R11 — Define what each verification step proves, and select an actual trajectory.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 74–75 and 126–134; [operations guide](fleet-manager/OPERATIONS.md), lines 25–29; [root README](README.md), lines 77–84.
  - **Problem:** The local guide says `doctor` does not upload; operations says storage-host failures pass it. In fact it sends a heartbeat through the upload path. Conversely, `active` is assigned during enrollment, and decrypting an arbitrary `install=` object can prove only heartbeat delivery or fail on plaintext `tags.json`. Real AWS is also not the only way to prove an upload: the next section does so with same-machine MinIO.
  - **Fix:** Define three checkpoints: `active` = enrolled identity; successful `doctor` storage check = heartbeat authorization and upload worked; a known session’s decrypted manifest/payload = actual collection worked. Identify or create a synthetic session, select its source-qualified `/mirror/source=<source>/…age` object, and verify it. Explain that loopback MinIO URLs cannot be used from another machine, while the same-machine path works. Reuse the walkthrough’s trajectory selection rather than suggesting any listed key.
  - **Evidence:** [Doctor](src/app/diagnose.go), lines 81–90; [upload test](src/e2e/vend_upload_test.go), lines 203–240; [walkthrough](fleet-manager/WALKTHROUGH.md), lines 118–141. The existing `age -d | zstd -d | tar -t` archive order is correct.

- **R12 — Declare the working directory and state location for every command sequence.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 26–45 and 179–182; [AWS README](fleet-manager/terraform/aws/README.md), lines 35–45 and 69–82; [GCP README](fleet-manager/terraform/gcp/README.md), lines 38–47 and 73–85.
  - **Problem:** Root and Fleet Manager `make build`/`make check` operate on different components. Manual Terraform blocks omit the starting directory. Own-image blocks change into `fleet-manager`, then issue `terraform apply` without returning to the initialized template/state directory. Raw `terraform output` does not locate the state created by the deployment script.
  - **Fix:** State the initial checkout location; use `make -C fleet-manager` or an explicit `cd`. Use explicit template directories or `terraform -chdir=...` throughout. Build images in a subshell or return to the same Terraform directory before applying. Use `deploy.sh <cloud> output ... --dir ...` for script-managed deployments. State that the image registry repository and push permissions must already exist, or show their setup.

- **R13 — Make AWS and GCP complete alternatives and list all verification prerequisites.**
  - **Where:** [Root README](README.md), lines 40–56 and 77–84; [local setup](fleet-manager/README.md), lines 87–90 and 130–134; [walkthrough](fleet-manager/WALKTHROUGH.md), lines 12–14.
  - **Problem:** AWS and GCP deployment commands appear consecutively without “choose one.” Being signed in to `gcloud` does not establish the ADC authentication checked by the script. Verification is AWS-only despite offering GCP. The walkthrough suggests adapting to GCP without supplying its authentication, outputs, storage, and versioned-cleanup sequence. Root/local prerequisites omit `zstd`; root also uses `age` after listing only `age-keygen`.
  - **Fix:** Label provider blocks as alternatives. State the ADC requirement and show `gcloud auth application-default login` before the GCP deployment. Include `age`, `age-keygen`, `zstd`, and `tar` for archive verification. Provide provider-specific verification commands. Either label the walkthrough AWS-only or write a complete GCP variant, including an isolated trial bucket and cleanup. Link cloud permission/network prerequisites before the first apply.
  - **Evidence:** [GCP preflight](fleet-manager/deploy.sh), lines 171–174; the [GCP README](fleet-manager/terraform/gcp/README.md), lines 31–36, already explains ADC correctly.

- **R14 — Isolate the local trial from an existing personal installation.**
  - **Where:** [Fleet Manager README](fleet-manager/README.md), lines 105–144.
  - **Problem:** The local recipe installs into the ordinary user’s binary/state/config locations, then purges those locations during cleanup. The installer preserves an existing enrollment, so supplying a trial invite does not necessarily join the trial. A newcomer with an installed shipper can replace its binary, use the wrong organization, or erase its state.
  - **Fix:** Require a fresh OS user/profile before these steps, or provide an isolated container/account recipe. Explain the existing-enrollment behavior before installation. Limit cleanup to the trial’s service, configuration, and state; do not tell a general reader to purge a pre-existing setup.
  - **Evidence:** [Installer](src/packaging/linux/install.sh), lines 27–29, 40–50; local cleanup uses `uninstall --purge` and removes the normal config file.

- **R15 — Replace the unconditional “rerun to upgrade” promise with a verifiable procedure.**
  - **Where:** [Root README](README.md), lines 61–62; [operations](fleet-manager/OPERATIONS.md), lines 57–61; [deploy.sh](fleet-manager/deploy.sh), lines 91–92 and 113.
  - **Problem:** Reapplying an unchanged `docker.io/quesma/fleet-manager:latest` string need not roll out changed image bytes. The script retains the recorded image and neither template resolves a registry digest or explicitly forces a rollout. This is a static implementation finding, not a live upgrade test. Cloud Run resolves tags into immutable revision digests and does not repull them when starting instances. See [Cloud Run deployment behavior](https://docs.cloud.google.com/run/docs/deploying).
  - **Fix:** Document how to obtain an approved image digest, deploy a new `--image ...@sha256:...`, verify the running revision/digest, and roll back. Alternatively implement explicit digest resolution/rollout before retaining the existing promise. Explain that Fleet Manager and shippers have separate update mechanisms.
  - **Evidence:** [AWS image variable](fleet-manager/terraform/aws/variables.tf), lines 7–10, and [service](fleet-manager/terraform/aws/main.tf), line 227; [GCP image variable](fleet-manager/terraform/gcp/variables.tf), lines 15–17, and [service](fleet-manager/terraform/gcp/main.tf), line 167.

- **R16 — State who owns updates and removal for each installation method.**
  - **Where:** [Root README](README.md), lines 95–110; [client README](src/README.md), lines 10–19, 42–47, and 123–131; [Homebrew README](src/packaging/homebrew/README.md), lines 12–23.
  - **Problem:** Root onboarding offers all-users installation and then promises self-update for every service. The generic client uninstall description promises binary removal, while Homebrew retains its managed binary. The macOS distinction already matters on `main`; pending Windows documentation adds the same ownership distinction.
  - **Fix:** Give a small ownership comparison before update/removal guidance: personal package/script installs self-update; Homebrew installs self-update but Brew owns binary removal; system packages are updated and removed by an administrator/MDM. Qualify the root self-update sentence and the generic uninstall description, and link the method-specific commands.
  - **Evidence:** [macOS MDM guide](src/packaging/macos/mdm/README.md), lines 71–81; [uninstall implementation](src/app/uninstall.go), lines 48–50, preserves Homebrew’s payload.

## P2: repair the remaining newcomer handoffs

- **R17 — Explain the macOS guide’s switch from Fleet Manager to Agent Statement.**
  - **Where:** [macOS MDM README](src/packaging/macos/mdm/README.md), lines 26–32 and 98–99.
  - **Problem:** The first enrollment-profile step says “In Agent Statement,” although the preceding journey introduced Fleet Manager. A reader cannot tell whether this is another mandatory service, another name for the UI, or an alternative control plane.
  - **Fix:** Use this repository’s Fleet Manager **Grants** page as the primary route and link to its instructions. If Agent Statement is intentionally supported, identify it explicitly as an alternative and explain where its URL/token come from. Link local Fleet Manager inventory documentation for the primary route; make external-product guidance optional.

- **R18 — Remove references to a configurable grant enrollment quota.**
  - **Where:** [Intune README](src/packaging/windows/intune/README.md), lines 27–29; pending `src/packaging/windows/intune/README-System.md`, lines 75–78.
  - **Problem:** The personal guide asks for a grant valid for a number of installs; the new system guide explicitly asks for an “enrollment limit.” Fleet Manager’s grant model/API/UI has no count-limit field. An operator cannot follow that instruction.
  - **Fix:** Describe an organization-scoped reusable grant that admits new enrollments until expiry or revocation, and distinguish it from a single-use invite. Link to grant creation. Describe distribution scope as an MDM rollout choice rather than a grant quota. If another control plane offers quotas, label that advice as specific to that implementation.
  - **Evidence:** [Grant record](fleet-manager/src/model.go), lines 159–166; [creation handler](fleet-manager/src/admin_http.go), lines 35–38; [manager](fleet-manager/src/manager.go), lines 131–145.

- **R19 — Teach how to stop tracking before explaining tracking exceptions.**
  - **Where:** [Client README](src/README.md), lines 92, 117–118, and 123–128.
  - **Problem:** “A repository marked as not tracked” appears before instructions for marking one. The usage list describes `tracking` only as a display, hiding its interactive collection control.
  - **Fix:** Add a short collection-controls section: inspect with `tracking`, select a repository and press `t`, explain the `.notrajectories` marker, and show pause/resume. State that stopping collection does not delete previous uploads. Then explain the existing remote/no-repository caveats. Keep the `preview` link beside this section so readers can inspect redacted content.
  - **Evidence:** [Tracking CLI](src/internal/cli/tracking.go), lines 27–29 and 201–214, documents the control and its non-retroactive behavior.

## Recommended document structure

- **Root README:** Product purpose and components → diagram/lifecycle → who can access/decrypt what → terminology and onboarding route chooser → concise operator/developer entry points → links to collection controls, operations, contribution, and license. Retain the current direct-upload arrow and role split. Make “binary per user installation / separately deployed service for the fleet / customer object store / separate reader” explicit within the first screen.
- **Fleet Manager README:** Responsibilities and durable state → choose cloud deployment or local evaluation → prerequisites and canonical setup link/sequence → organization and age custody → invites/grants → verification checkpoints → operations links → development → API reference links. Move detailed tags schemas, inventory matching rules, and telemetry storage mechanics out of the main setup narrative.
- **Client README:** Installation methods and ownership → collection inventory/privacy exceptions → inspect/pause/exclude → ordinary usage → pipeline → configuration/security reference → building. Keep its concrete source inventory and local-progress explanation.
- **Provider/MDM READMEs:** State audience, required existing Fleet Manager setup, starting directory, and expected credentials first; provide one complete sequence with expected outputs and the next step. Keep Homebrew’s contributor/distribution material distinct from user installation instructions.

## Completion criteria for the documentation fixes

- A newcomer can explain where each process runs; why Fleet Manager is required for a normal fleet; who owns the bucket; why ciphertext bypasses the manager; which keys decrypt; and why cloud read permission is different from decryption ability.
- An operator can follow one explicitly chosen provider path from prerequisites to a decrypted known session without inventing commands, changing directories implicitly, or confusing `active` with successful collection.
- A developer can identify what is collected, understand the account-data exception, inspect/pause/exclude collection, and determine who updates/removes their installation.
- A shared-machine rollout correctly uses separate per-user enrollments and supported grants; neither the MDM nor Intune guide introduces an unexplained service or nonexistent quota.
- Local evaluation and failed cloud cleanup preserve any pre-existing installation and the state needed to recover surviving cloud resources.
- Fixes reconcile linked security/operations text as well as the READMEs, so following a link does not reverse the explanation just learned.

## Validation and limits of this review

- Local README link/heading checks found no missing targets or anchors in the reviewed checkout. Navigation findings concern discoverability, ordering, and meaning rather than broken links.
- Source/templates and existing tests were inspected for factual findings. External checks were limited to the cited primary Terraform/OpenTofu and Cloud Run documentation.
- `make check` passed, including race tests, in the isolated report branch based on `eaef886`. This validates that branch’s baseline, not the separate in-progress Windows implementation or cloud deployment recipes. The change adds only this root report; no Fleet Manager file, fixture, protocol, grant, or performance budget changes.
- No cloud resources were created or destroyed; installers and destructive cleanup were not executed. Actual cloud provisioning, release/download availability, and a live upgrade remain untested. Performance testing is not applicable to a report-only change.
- The Windows installer-tests README clearly identifies its maintainer audience; no substantive newcomer-structure finding was identified there.
