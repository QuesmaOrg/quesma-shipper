# Security

## Report a vulnerability

Do not open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting at
<https://github.com/QuesmaOrg/quesma-shipper/security/advisories/new>, or email
`contact@quesma.com` with the subject `Security: fleet-manager`. Include:

- the version, from running `fleet-manager` with no arguments, and the provider (`aws`, `gcp` or
  `azure`);
- the impact and the conditions needed to reproduce it;
- minimal reproduction steps that use synthetic data;
- a suggested mitigation, if you have one.

Do not include real bucket names, account identifiers, administrator credentials, invite secrets,
presigned URLs, `age` identities or trajectory data in the report.

We acknowledge each report, assess it, and coordinate disclosure with you. A fleet manager is
deployed by its operator rather than updated by us, so allow time for a fixed image to be published
and for operators to deploy it before you publish details.

## Scope

In scope:

- **Credential handling.** A path that accepts a wrong administrator, reporter or invite
  credential. An invite that can be used twice. A revoked install that is still served
  configuration or still granted upload tickets.
- **Organization isolation.** A credential or an install scoped to one organization that reaches
  another organization's control records, configuration or objects.
- **The upload ticket.** A ticket that authorizes a key outside the requesting install's own
  prefix — one install overwriting another's objects is the invariant here. A signed header outside
  the ticket contract in `src/aws.go`. A ticket whose lifetime exceeds what was asked for.
- **Configuration authority.** A served document that changes a field the
  [shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) rulebook does not permit. A
  served recipient list the organization did not set — including the built-in ETL recipient
  appearing while `allow_quesma_etl` is off, or was never set on a deployment whose
  `FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL` is `false`.
- **The administration surface.** Anything under `/v1/admin` or `/admin/` reachable without the
  deployment credential. A reporter credential that reaches an administrator operation. The
  credential escaping tab-scoped `sessionStorage`, or being sent anywhere but the `Authorization`
  header.
- **Telemetry forwarding.** Forwarding to a collector the organization did not configure. Relaying
  a collector's response body. Any path that exposes the signing seed at
  `private/fleet-manager/telemetry-identity.json`.
- **The deployment templates.** A grant in `terraform/aws` or `terraform/gcp` wider than the
  service needs — in particular anything that lets the runtime identity read a trajectory payload.

Out of scope:

- The shipper itself. It has [its own repository and its own
  policy](https://github.com/QuesmaOrg/quesma-shipper/blob/main/SECURITY.md).
- Anything that needs an attacker who already holds the deployment's cloud credentials, its
  Terraform state, or its administrator credential.
- The design trade-offs the [README](README.md) already states: that the runtime identity may list
  keys below `v1/organization=`, which reveals encrypted object names but not their contents; and
  that there is no organization-deletion operation.

## Handle data safely

This service holds enrollment credentials, install identities and every organization's
configuration. It does not hold trajectories — those go straight from the machine to the bucket,
sealed to the organization's `age` recipients — but it sits beside them, and a deployment's bucket
holds both.

- Do not attach real control records, logs with real identifiers, or bucket listings to an issue or
  pull request.
- Keep private `age` identities, administrator credentials and Terraform state out of the checkout,
  shell history, CI artifacts and command-line arguments.
- Treat the telemetry signing seed as a key. It lives outside `v1/` precisely so that an archive
  reader granted `v1/*` cannot see it; do not copy it into archive mirrors.

## Supported versions

Only the newest release receives fixes. A deployment pulls an image, so upgrading is the operator's
action: redeploy and restart. The service is stateless and its records are conditional-write
guarded, so a rolling replacement is safe.

## Design guarantees

A security-relevant change must preserve the articles in the shipper's
[CONSTITUTION.md](https://github.com/QuesmaOrg/quesma-shipper/blob/main/CONSTITUTION.md) and the
invariants in [ARCHITECTURE.md](ARCHITECTURE.md). The control plane never receives trajectory
bytes. No install can overwrite another's objects. Configuration cannot turn off scrubbing or
encryption. The service has no decrypt path and no delete operation.
