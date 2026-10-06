# Security policy

## Report a vulnerability

Do not open a public issue for a suspected vulnerability. Do not include real credentials, customer
data, presigned URLs, or production payloads in a report.

Use GitHub's private vulnerability reporting at
<https://github.com/QuesmaOrg/shipper-protocol/security/advisories/new>, or email
`contact@quesma.com` with the subject `Security: shipper-protocol`. Include:

- the affected protocol or module version;
- the impact and the conditions needed to reproduce it;
- minimal reproduction steps that use synthetic data;
- a suggested mitigation, if you have one.

We acknowledge each report, assess it, and coordinate disclosure with you. Allow time for both
protocol peers to be patched before you publish details.

## Supported versions

Security fixes are made on the latest released module version. The default branch is development
code and is not a supported release. Wire versions are immutable after release. A fix that cannot
be made compatibly uses a new endpoint and schema version.
