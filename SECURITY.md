# Security

## Report a vulnerability

Do not open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting at
<https://github.com/QuesmaOrg/quesma-shipper/security/advisories/new>, or email
`contact@quesma.com` with the subject `Security: quesma-shipper`. Include:

- the shipper version, from `quesma-shipper --version`, and the OS;
- the impact and the conditions needed to reproduce it;
- minimal reproduction steps that use synthetic data;
- a suggested mitigation, if you have one.

Do not include real trajectories, credentials, presigned URLs, or exploit code in the report.

We acknowledge each report, assess it, and coordinate disclosure with you. Allow time for a fix to
reach installed clients through self-update before you publish details.

## Scope

In scope:

- Scrub bypass. A secret or personal-data pattern that the compiled rule packs must catch and do
  not. A way to make a file skip scrubbing.
- Encryption. A path where plaintext or a decrypting key leaves the machine. The client accepting
  recipients it must not.
- Upload. A way for one install to overwrite another install's objects. An upload to an origin
  outside the allowlist.
- Configuration authority. A served document that changes a field the rulebook does not permit.
- Self-update. Installation of a binary that is not signed through the TUF chain. A downgrade.
- Local state. Files written with wrong permissions. Writes inside an agent's own store.

Out of scope:

<!-- TODO(control-plane-oss): link the control-plane repository here. https://github.com/QuesmaOrg/quesma-shipper/issues/1 -->
- The control plane and the storage reader. They are not part of this repository.
- Issues that need an attacker who already controls the user account on the machine.

## Handle data safely

Trajectory files contain prompts, source code, shell output, and often credentials. The shipper
also collects context artifacts, a project-to-repository map, and account metadata; the README
section [What is collected](README.md#what-is-collected) lists them. Treat local state, logs, and
encrypted bundles as confidential.

- Do not attach real trajectories, agent databases, logs, credentials, or encrypted bundles to an
  issue or pull request.
- Use synthetic fixtures. If real data is essential to reproduce a problem, minimise and redact it
  locally first. Send it through the private report.
- Keep private keys and update-signing material out of the checkout, shell history, CI artifacts,
  and command-line arguments.

## Supported versions

Only the newest release receives fixes. Release builds self-update. If you disabled self-update
with `SHIPPER_NO_SELFUPDATE`, run `quesma-shipper update` after a security release.

## Design guarantees

A security-relevant change must preserve the articles in [CONSTITUTION.md](CONSTITUTION.md). Every
file is scrubbed and then encrypted before upload. The control plane never receives trajectory
bytes. One install never overwrites another. Upload progress is recorded locally. The update chain
is anchored to an offline-signed TUF root embedded in the binary.
