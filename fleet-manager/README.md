# Fleet manager

`fleet-manager` is the control plane for [Quesma Shipper](../README.md), which lives beside it in
this repository: a multi-organization control service. It stores configuration, enrollment
credentials, and install identities below `v1/organization=<org>/control/`. Trajectory payloads go
directly from shippers to object storage; the manager has no decrypt, acknowledgement, or cursor
path for them. Outside `control/` it reads only each install's `tags.json` (see
[Install names](#install-names)) and the metadata of mirror objects, for upload deduplication.

The service exposes shipper endpoints, a bearer-authenticated administration API below `/v1/admin`,
and a self-contained administration UI at `/admin/`. Static UI assets are embedded in the binary and
make no third-party requests.

To deploy it in your own cloud, follow [Run Fleet Manager](../README.md#operator-set-up-fleet-manager-once) in the
repository README, which is one script over the templates under [terraform/](terraform/); for
everything on a laptop, [On one machine](#on-one-machine) below. Once it runs,
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

## On one machine

MinIO, Fleet Manager and a shipper on a single machine, built from this repository, for evaluating
the system, developing against it, or demonstrating it. Nothing connects to a cloud provider. You
need Docker (for MinIO), Go 1.27 or newer, the AWS CLI, `age` and `age-keygen`, `openssl` and
`curl`, on macOS or Linux.

```sh
git clone https://github.com/QuesmaOrg/quesma-shipper
cd quesma-shipper
make -C fleet-manager run
```

This is [Run it](#run-it) above: a MinIO container named `fleet-minio` on `127.0.0.1:9000`, the
`trajectories` bucket with versioning on, an administrator credential, and Fleet Manager on port
8099 in the foreground, printing the credential and the admin UI address. It listens on every
interface over plain HTTP, so use a trusted network. Leave it running and continue in a second
terminal. Create the organization and an invite as in
[Operator: set up Fleet Manager](../README.md#operator-set-up-fleet-manager-once), steps 2, 4 and 5.

**Pin the upload target before enrolling.** The local MinIO is plain HTTP and addressed path-style,
and the shipper refuses an upload ticket that is not HTTPS unless told otherwise, in its user
configuration, `~/.config/trajectory-shipper/config.yaml`:

```yaml
upload_targets:
  - origin: http://127.0.0.1:9000
    addressing: path-style
    path_prefix: /trajectories
    allow_loopback_http: true
```

Then build the shipper, install your build with its background service, and enroll, in one
command; `--from` uses your binary instead of downloading a release, on Linux and on macOS:

```sh
make build
sh src/packaging/linux/install.sh --from bin/quesma-shipper fmi2.… --server http://127.0.0.1:8099
~/.local/bin/quesma-shipper doctor
```

`doctor` confirms enrollment, not an upload. The service ships on start and then every 15 minutes;
list what it uploaded with the local store's credentials, then open one object with a custodian's
identity:

```sh
export AWS_ACCESS_KEY_ID=localadmin AWS_SECRET_ACCESS_KEY=localadmin-secret AWS_REGION=us-east-1
aws s3 ls --recursive --endpoint-url http://127.0.0.1:9000 s3://trajectories/v1/organization=acme/install=
aws s3 cp --endpoint-url http://127.0.0.1:9000 "s3://trajectories/<key from the listing>" object.age
age -d -i acme-security.agekey object.age | zstd -d | tar -t     # manifest.json, payload
```

When you are done, remove the shipper and its service first, so it stops trying to upload, then
the pin, then the control plane and its storage:

```sh
~/.local/bin/quesma-shipper uninstall --purge     # --purge also deletes enrollment and local state
rm ~/.config/trajectory-shipper/config.yaml        # the pin, if nothing else is in the file
docker rm -f fleet-minio                           # after Ctrl-C in the Fleet Manager terminal
rm -rf fleet-manager/data/
```

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

The templates, for running them by hand: [AWS](terraform/aws/README.md) and
[Google Cloud](terraform/gcp/README.md).

## Install names

An install id is a UUID baked into every object key, and nothing a shipper uploads says who holds
the machine. The administration UI carries a name and optional administrator-managed metadata per install, stored as

```
v1/organization=<org>/install=<install-id>/tags.json
```

```json
{"schema": 1, "install_id": "…", "name": "Rafal's laptop", "metadata": {"email": "rafal@example.com", "mdm": "jamf"}, "updated_at": "…"}
```

It sits in the install's own root, beside the objects it names, so a tool walking the bucket can
resolve an id without asking this service or holding a database credential. That is the whole
reason it is not another record under `control/`.

`GET /v1/admin/orgs/{org}/installs/tags` returns every existing tags record, read the way the seen
records are: one request, fanned out over the installs list, off the critical path so the table
renders first. Before metadata, the list held only named installs; now a record may carry metadata
and no name, so `name` is optional and a client must not assume a listed install has one.
`PUT /v1/admin/orgs/{org}/installs/{id}/tags` sets one, and an empty name clears it back to the id.
Send `{"name":"Rafal's laptop","if_missing":true}` to fill a missing name while preserving a custom
name atomically. A revoked install can still be named — the objects it already wrote still want a
label.

### Metadata and MDM inventory

Use **Installs → Metadata** to add, edit, or remove fields. Only administrator credentials may
write these fields; shipper and reporter credentials cannot. Metadata has at most 16 keys matching
`^[a-z][a-z0-9_]{0,31}$`; values are strings of at most 256 Unicode characters without control
characters. `email`, `department`, and `mdm` are useful conventions, not reserved fields.

`PATCH /v1/admin/orgs/{org}/installs/{id}/metadata` accepts a partial update:

```json
{"metadata":{"email":"rafal@example.com","department":"Engineering","old_field":null}}
```

A string sets one key and `null` removes it; omitted keys stay intact. Naming and metadata updates
use conditional writes with retries, preserving concurrent changes to other fields. Conflicting
edits to the same field follow the order of successful writes. Clearing all fields retains the
existing tags record. Identity records and shipper protocol messages remain separate.

Use **Import MDM inventory** to upload or paste CSV, with `hostname` first and metadata keys as the
remaining headers. Up to 1000 rows and 1 MiB are accepted. Quote CSV fields containing commas:

```csv
hostname,email,department,mdm
Rafal-MacBook,rafal@example.com,Engineering,jamf
```

The API equivalent is `POST /v1/admin/orgs/{org}/installs/metadata/import`:

```json
{"rows":[{"hostname":"Rafal-MacBook","metadata":{"email":"rafal@example.com","department":"Engineering","mdm":"jamf"}}]}
```

Imports merge the supplied keys. A row is matched to installs of the selected organization by
hostname, compared case-insensitively on the first DNS label: a shipper enrolls with what
`os.Hostname()` reports, which on a Mac is the Bonjour name (`Alices-MacBook.local`) and on a
managed network can be a full DNS name, while an MDM export carries the computer name as the
device manager kept it (`alices-macbook`). A row imports automatically only when exactly one install
matches and the inventory contains one row for that hostname; two machines whose names fold to the
same key are ambiguous, never guessed. Pending and revoked identities count as matches, so a
replaced install or several users sharing a hostname cannot silently select an identity. The
response contains `results`, each with its one-based `row`, `hostname`, `status` (`imported`, `unmatched`, `ambiguous`, or
`error`), and optional `install_id`, matching `candidates`, and `message`.

The UI reports each result and provides install selectors for unresolved rows. Choose distinct
installs and click **Apply selected rows**, or leave a row unselected to skip it. The API supports the
same manual choice by including `install_id` in a resubmitted row; that ID must belong to the
organization. Multiple rows targeting the same install in a request are reported as ambiguous.
Imported names and unrelated metadata are retained. An import is not a transaction across installs:
only retry rows that did not succeed. Invalid row data is rejected before any writes; storage errors
are reported per row.

Existing tags without metadata remain valid. Older fleet-manager releases use a strict tags decoder
and cannot read records containing metadata; upgrade all replicas before importing and keep this in
mind when rolling back.

The read shares a grant with upload deduplication, which HEADs mirror objects for their
`source-hash` and `shipped-hash`: `InstallRead` on AWS and the `data` role on GCP cover the whole
install prefix, because S3 and GCS authorize a HEAD as a full read. The runtime can therefore
fetch every sealed payload; it holds no age identity, so it cannot open one. Unlike sealed payloads,
`tags.json` carries readable personal data such as an owner's email, and whoever holds the read (an
organization's own tooling, ingest-etl, and Quesma where an organization has granted it the read)
reads that too. Put into metadata what you are content for every holder of that grant to see.

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

## Source catalogs

A shipper build that has one reports its compiled source catalog on every config fetch: the
sources it can collect, its scrub rule packs, and the served-document features it reads (see
"Source catalog report" in the shipper-protocol `PROTOCOL.md`). Root templates arrive unexpanded,
so a catalog is the same on every machine running that build and says nothing about the machine or
its files. The service keeps each install's latest one, content-addressed:

```
v1/organization=<org>/control/catalogs/<sha256>.json   one per distinct catalog, written once
```

The SHA-256 is over the catalog as this service re-encodes it, so fields it does not know are never
stored. The install's check-in record names the digest its latest config fetch carried
(`catalog_digest`, reported at `last_config_at`), and drops it when a fetch carries none. A
changed digest is written even inside the one-minute check-in throttle. A catalog without source
ids is ignored and logged, and the fetch is served as one without a catalog. Catalogs sit under
`control/`, so the ETL reader grant, which names install roots and `config.json` only, does not
reach them.

**Serving per install.** With the requesting build's catalog, the document is rendered for that
build: a `sources[]` entry or `scrub.rule_packs` name the catalog lacks is left out, so the build
does not refuse the whole document over an entry another build needs. `sources[].exclude_add` is
served as is to a build listing the `sources.exclude_add` feature. For one that does not, which
would ignore it, it is folded: `exclude` becomes the entry's own `exclude` if it sets one, else the
build's reported `exclude` for the source, followed by the additions. A fetch without a catalog is
served as before, except that `exclude_add` is folded over the newest catalog any install of the
organization reported for that source; if none has the source, the `exclude_add` is dropped. What
was left out, folded or dropped is logged once per change for each install, not on every fetch.

**What installs report.** `GET /v1/admin/orgs/{org}/sources` answers what the active installs can
collect:

```json
{
  "installs": {"active": 14, "reporting": 12},
  "sources": [{"id": "claude-code-transcripts", "family": "claude-code", "family_name": "Claude Code",
               "description": "…", "artifact_class": "trajectory", "enabled": true, "roots": ["~/.claude"],
               "include": ["…"], "exclude": ["…"], "max_file_bytes": 536870912, "enrichers": [], "installs": 12}],
  "rule_packs": [{"name": "gitleaks-core", "installs": 12}],
  "features": [{"name": "sources.exclude_add", "installs": 12}]
}
```

`active` counts installs with status active; `reporting` those of them whose latest config fetch
carried a catalog. Pending and revoked installs count for neither. A source, pack or feature is
listed when a reporting install has it, `installs` is how many do, and a source's metadata comes
from the most recently reported catalog that has it. Sources sort by `family_name`, then `id`;
packs and features by name. Lists are empty arrays, never null, and `max_file_bytes` is absent
when the catalog states none.

**Checking writes.** A `PUT /v1/admin/orgs/{org}/config` that states a collection is checked, after
the schema, against the same counts:

- While any install reports, every `sources[].id` and `scrub.rule_packs` name must be in some
  reporting install's catalog, else 400, for example
  `collection: sources[2].id "foo" is not in any reporting install's catalog`. With none reporting,
  they are not checked.
- Any `sources[].exclude_add` needs every active install to report its catalog, else 409:
  `collection: sources[].exclude_add needs every active install to report its catalog; 2 of 14 do
  not yet. Update their shippers first.` A build without a catalog cannot be folded for reliably,
  so the write waits for the fleet to update.

A write that does not mention the collection keeps the stored one and is not checked, so a
recipient change is never refused because the fleet moved. Older fleet-manager releases decode
check-in and configuration records strictly: upgrade every replica before installs report
catalogs or a collection uses `exclude_add`, and keep this in mind when rolling back.

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
