# Fleet manager

`fleet-manager` is the control plane for [Quesma Shipper](../README.md), which lives beside it in
this repository: a multi-organization control service. It stores configuration, enrollment
credentials, and install identities below `v1/organization=<org>/control/`. Trajectory payloads go
directly from shippers to object storage; the manager has no decrypt, acknowledgement, or cursor
path for them, and the only object it reads outside `control/` is each install's `tags.json` — see
[Install names](#install-names).

The service exposes shipper endpoints, a bearer-authenticated administration API below `/v1/admin`,
and a self-contained administration UI at `/admin/`. Static UI assets are embedded in the binary and
make no third-party requests.

To deploy it together with the shippers, on one machine or in your own cloud, follow
[Get started](../README.md#get-started) in the repository README; once it runs,
[OPERATIONS.md](OPERATIONS.md) covers operating it. This README covers what the service does and
how to work on it.

## Status

Pre-1.0. The wire protocol, the configuration format and the object naming are versioned and
pinned by tests, but they can still change between minor releases. There is no organization
deletion and no migration tooling.

## Develop

```sh
make check        # the commit gate: gofmt, vet, VERSION, dependency licenses, Go and UI tests
make build        # the binary
make vulncheck    # govulncheck over the module on its own, with the workspace off
```

This needs Go 1.27 and Node 22 or newer; the browser script checks use Node's built-in test runner.
UI assets are embedded, so rebuild and restart the local server after editing them.
[ARCHITECTURE.md](ARCHITECTURE.md) states the invariants a change has to preserve, and the
repository's [CONTRIBUTING.md](../CONTRIBUTING.md) covers the rest.

### Run it

```sh
make setup        # start a local MinIO, create the bucket, enable versioning, mint a credential
make build        # the binary
make run          # serve on port 8099, every interface
```

`run` pulls the other two in, so `make run` on a clean checkout is the whole thing. `setup` is
idempotent: it reuses an existing bucket and credential rather than replacing them. It prints the
administrator credential to paste into the admin UI at `http://127.0.0.1:8099/admin/`.

`setup` records the store it provisioned in `data/config.mk`, so `run` serves that one without you
repeating the variables. A variable on the command line still wins; delete `data/` to start over.

Stop the server with Ctrl-C, and the store with `docker rm -f fleet-minio`.

### Run it against real AWS

Leave `S3_ENDPOINT` empty and the standard credential chain is used instead of the local keys, with
no endpoint override anywhere. Name the bucket and region on the first `setup`; it records them, so
the later commands need no arguments:

```sh
aws sts get-caller-identity          # check which account you are pointed at

make setup S3_ENDPOINT= S3_REGION=eu-central-1 DEV_BUCKET=acme-trajectories
make build
make run
```

`setup` creates a real bucket in whichever account the profile resolves to, and does not start a
store for you — that part is AWS's. It refuses the default bucket name here, because `trajectories`
is neither yours nor globally unique.

This is the only local arrangement where a presigned upload URL names a host another machine can
reach, which makes it the one that can prove an upload end to end.

| Knob | Default | |
| --- | --- | --- |
| `S3_ENDPOINT` | `http://127.0.0.1:9000` | empty means real AWS |
| `S3_REGION` | `us-east-1` | |
| `DEV_BUCKET` | `trajectories` | required against real AWS |
| `DEV_PORT` | `8099` | |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | `localadmin` / `localadmin-secret` | ignored against real AWS |

## Executable mode

Run the service explicitly:

```sh
fleet-manager --serve --provider aws --bucket acme-trajectories --region eu-central-1
```

`PORT` wins over `AWS_LWA_PORT`; the fallback is 8080. Running the executable without arguments
prints its version and concise help, then exits successfully without loading cloud credentials or
opening a listener. There are no local administrative commands. The process itself serves HTTP and
must run behind a trusted HTTPS terminator; never expose `/admin/` or `/v1/admin` over cleartext.

Server flags can also be supplied with environment variables:

| Flag | Environment | Use |
| --- | --- | --- |
| `--provider` | `FLEET_MANAGER_PROVIDER` | `aws`, `gcp`, or `azure` |
| `--bucket` | `FLEET_MANAGER_BUCKET` | S3/GCS bucket or Azure container |
| `--region` | `FLEET_MANAGER_REGION` | AWS region |
| `--account` | `FLEET_MANAGER_ACCOUNT` | GCP signing service account or Azure storage account |

Azure also requires `FLEET_MANAGER_AZURE_SUBSCRIPTION_ID` and
`FLEET_MANAGER_AZURE_RESOURCE_GROUP` to verify object versioning through Azure Resource Manager.
Azure infrastructure is intentionally deferred.

## Administration

The AWS and GCP templates publish the administration UI's address and the credential it asks
for as Terraform outputs, among others (`service_url`, `bucket`, `reporter_credential`,
`fleet_manager_public_key`, ...; see each template's `outputs.tf`):

```sh
terraform output -raw admin_url
terraform output -raw admin_credential
```

Open `admin_url`, paste the deployment-wide sensitive credential, and use the UI to create and
switch organizations; create, list, and revoke grants and invites; release recoverable invite reservations; and list or
revoke installs. The credential stays in tab-scoped `sessionStorage`, is sent only in the
`Authorization` header, and is removed on logout or when the tab closes.

Initialization requires at least two distinct public `age` recipients. Custodians should generate
and retain their private identities outside fleet manager, for example:

```sh
age-keygen -o acme-security.agekey
age-keygen -y acme-security.agekey
```

Paste only the printed `age1…` public recipient into the UI. Protect Terraform state, the
administrator credential, and private `age` identities according to the organization's backup and
access-control requirements.

Quesma ETL decryption is on unless an organization turns it off, or the deployment turns it off for
every organization that never chose (`default_allow_quesma_etl = false` in the Terraform). While
on, the manager adds Quesma's built-in public recipient to future shipper configurations; it never
holds the private identity and still has no permission to read trajectory objects. Quesma can
access those ciphertexts only when the customer separately provides bucket access or sends selected
objects. It is on by default because sealing cannot be added later: turning the recipient on
covers only uploads from then on, so analytics over an organization's history needs it from the
start. Turning it off removes the recipient from future configurations and makes Quesma ETL,
dashboards, and dependent features unavailable for those uploads; objects already sealed to it stay
openable by it.

Deployment runbooks:

- [Google Cloud](terraform/gcp/README.md)
- [AWS](terraform/aws/README.md)

## Install names

An install id is a UUID baked into every object key, and nothing a shipper uploads says who holds
the machine. The administration UI therefore carries a name per install, stored as

```
v1/organization=<org>/install=<install-id>/tags.json
```

```json
{"schema": 1, "install_id": "…", "name": "Rafal's laptop", "updated_at": "…"}
```

It sits in the install's own root, beside the objects it names, so a tool walking the bucket can
resolve an id without asking this service or holding a database credential. That is the whole
reason it is not another record under `control/`.

`GET /v1/admin/orgs/{org}/installs/tags` returns the fleet's names, read the way the seen records
are: one request, fanned out over the installs list, off the critical path so the table renders
first. `PUT /v1/admin/orgs/{org}/installs/{id}/tags` sets one, and an empty name clears it back to
the id. A revoked install can still be named — the objects it already wrote still want a label.

The read costs one narrow grant. The runtime identity is otherwise write-only below `install=`, and
the templates hold it to the single object name (`InstallNamesRead` on AWS, the `names` role on
GCP) rather than the prefix, which would be read access to every sealed payload.

## Install telemetry

Every shipper request carries `X-Shipper-Version`, `X-Shipper-OS`, and `X-Shipper-Boot`. The service
records them, plus the times of the last config fetch and the last upload authorization, under
`v1/organization=<org>/control/seen/<install-id>.json`. The administration UI reads them from
`GET /v1/admin/orgs/{org}/installs/seen`, separately from the installs list, so a large fleet's table
renders before its detail columns arrive.

These records are disposable and deliberately separate from the install identity: a failed telemetry
write never fails a shipper's request, and repeated check-ins within a minute collapse into one
write. "Last vend" means tickets were issued, not that the upload finished — files go straight from
the machine to the bucket, which this service never sees.

Every write still leaves a superseded version in a versioned bucket. The AWS template tags these
objects `lifecycle=ephemeral` and expires their superseded versions the next day. GCS has no
tag-based lifecycle condition and its prefix conditions take no wildcard, so a GCP deployment
accumulates these versions; expire them with an out-of-band job if that matters for the fleet's size.

The GCP runtime role includes `storage.objects.delete` only on every organization's control prefix
and install-data prefix because GCS requires delete-plus-create to replace object bytes. Bucket
versioning preserves the displaced version, and fleet manager itself exposes no delete operation.
Organization discovery also permits the runtime identity to list keys below `v1/organization=`.
That listing can reveal encrypted trajectory object names, but not read their contents; this is an
accepted tradeoff of using each organization's `config.json` as the only organization registry.

## Telemetry proxy

`POST /v1/telemetry` forwards authenticated shipper telemetry to the organization's
`telemetry_collector_url`, signed with a deployment key that fleet-manager provisions on startup.

## Contributing

One system, one set of rules, kept at the repository root for the client and this service alike.
[CONSTITUTION.md](../CONSTITUTION.md) is the design authority, [CONTRIBUTING.md](../CONTRIBUTING.md)
covers setup and the commit gate, and all participation is subject to the
[code of conduct](../CODE_OF_CONDUCT.md).

## Security

Do not open a public issue for a suspected vulnerability. [SECURITY.md](SECURITY.md) has the
disclosure process and states what is in scope.

## License

[Apache License 2.0](LICENSE). Third-party notices are in [NOTICE](NOTICE), and every dependency's
license text is under [third_party/](third_party/).

The Quesma name and logo are trademarks and are not covered by that license. See
[TRADEMARKS.md](../TRADEMARKS.md): you may fork and run this freely; a redistributed fork carries a
different name.
