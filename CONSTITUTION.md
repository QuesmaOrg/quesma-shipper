# Constitution

This is the sole design authority for the trajectory shipper and its control plane.
Anything not written here is an implementation choice, changeable through an
ordinary PR.

Changing this document is an amendment: its own PR, trade-off argued in the
description.

## Articles

### 1. A fleet, managed centrally

Many shipper installs, managed by a control-plane server — customer-hosted or our
SaaS. A shipper obtains its configuration, including what it collects, from the
control plane; running without one is allowed only for local development and
testing. At enrollment a client mints its own identity or obtains one from the
control plane; both paths are supported. Features are judged by how they behave at
fleet scale. (Repealed: v1's compiled-in scope ceiling. Collection scope is central
configuration, not a build property.)

### 2. The control plane never touches the data

Collected files upload directly from the client machine to the sink. The control
plane does not proxy, receive, or store trajectory bytes.

### 3. Open to modification

Shipper and control plane expose hooks on specific actions, points where behavior
can be swapped, and configuration over hard-coded policy. Where v1 fixed a choice —
the sink auth method, for instance — the default posture is configurable; a hard
rejection needs an article here to stand on.

### 4. Nothing ships raw: scrubbed, then encrypted

Every trajectory file is scrubbed before upload: credentials, PII, whatever the
operator needs removed. The stage is a fixture of the pipeline; what it scrubs is
configurable and a hook point (Article 3). What survives ships encrypted with
`age`. The control plane supplies the recipients — possibly several — and the
client encrypts to them. The client needs no ability to decrypt what it ships;
encrypt-only installs are the expected shape.

### 5. Shippers never overwrite each other

No shipper may overwrite another shipper's files, regardless of sink or auth
method. A destination or configuration that cannot uphold this is invalid. The
enforcement mechanism (conditional writes, key layout, other) is an implementation
choice.

### 6. Sinks are plugins; auth is the sink's business

Authentication to a sink is sink-specific, configured per organization. Vended
short-lived STS credentials, presigned URLs, static keys — none is privileged; a
sink plugin brings whatever its store needs. (Repealed: v1's hard rejection of
per-object presigned URLs.)

### 7. Upload progress is tracked locally

Each install keeps its own record of what has been shipped, committed only after
the destination confirms the write. That local record is the authority: a shipper
resumes from its own state, and losing the control plane loses no knowledge of
what was uploaded. The record's shape and location are implementation choices.

## Candidates under discussion

Not yet articles. New topics land here first, then get adopted, reshaped, or
explicitly rejected. Nothing pending.

## What else governs

- [ARCHITECTURE.md](ARCHITECTURE.md) — code-level layering rules, enforced as
  tests under `src/`, including `src/internal/platform/writepath_lint_test.go`. It
  governs how code is arranged;
  this document governs what the product assumes.
- [shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) — the shipper ⇄ control-plane wire
  contract: message schemas, golden fixtures, and the served config's authority
  rulebook, released as a Go module and enforced here by contract tests. Where the
  doc and the tests disagree, the tests are right.
- [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md) — repo hygiene
  and disclosure.
