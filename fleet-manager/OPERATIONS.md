# Operating Fleet Manager

What to know once Fleet Manager is running: why the installation order matters, the day-to-day
operations, and how to prepare storage by hand when you use neither `make run` nor the Terraform
templates. The installation itself is in the repository [README](../README.md#operator-set-up-fleet-manager-once).

## Why the order is what it is

Install in the order given: storage, then Fleet Manager, then the shippers. Each stage depends on
the previous one. Four of these dependencies are enforced by the software, and three of them report
an error at a later stage than the one where the cause is fixed.

**Object versioning is checked when the first organization is created, not at startup.** A bucket
without versioning starts the service normally, then refuses to create an organization with
`{"error":"create organization unavailable"}`. The reason appears only in the service log, as
`admin create organization failed: object versioning is required`, so check the log rather than the
response. `make run` and the Terraform templates both enable versioning, so this applies only if you
create the bucket by hand.

**The telemetry signing identity is created at startup, before the listener opens.** The runtime
identity needs both read and write access to `private/fleet-manager/telemetry-identity.json`,
including in deployments that forward no telemetry. A read-only grant stops the service from
starting.

**The storage hostname is fixed by the first upload, not by the first deployment.** The host is
part of what a presigned PUT signature covers, and there is no separate setting for an internal and
a public endpoint. Object storage must resolve to the same hostname for Fleet Manager and for every
developer machine. This error passes the `doctor` check and only shows when nothing arrives in the
bucket.

**Files are sealed to the recipients in force at upload.** Changing the recipients later does
not re-encrypt existing files during normal runs, even after a shipper's `state reset`: upload
deduplication still skips a matching original file and scrubbed payload.

To re-encrypt from source on one enrolled machine **without a custodian decryption identity**,
first upgrade Fleet Manager to support `/v3/uploads/authorize`, then run locally:

```sh
quesma-shipper run --once --force-resend
```

The command requires a successful, unexpired served-config fetch during this invocation. It refuses
cached or missing configuration even if upload authorization works. Before uploading it prints the
recipient count and age public keys. Check these against the intended readers. Fleet Manager logs
each authorized forced batch with the install ID and object count. Forced uploads use v3; an older server returns
an upgrade error. Normal uploads and heartbeats continue using v2.

A successful forced PUT records a digest of the recipient set and scrub policy in the local
fingerprint. Retrying the same policy skips unchanged files already completed, including derived
outputs; a changed policy or changed content uploads again. Missing digests require one forced PUT.
Ordinary runs keep their existing duplicate detection and do not initiate a policy-driven resend.
The fleet-wide trigger remains separate work in [#77](https://github.com/QuesmaOrg/quesma-shipper/issues/77).
There is no remote resend setting, UI action, or stored resend job.

Each attempt drains beyond `max_files_per_run`, bounded by `drain_deadline` (5 minutes by default).
Sources with enrichment run in slices bounded by `max_files_per_run` and the in-flight byte cap,
so raw transcript memory does not grow with the entire history. Each slice commits before the next.
Pause, exclusions, size limits, scrubbing and install isolation still apply. Failures, parked files,
and deadline expiry return an error. Fix the reported cause and rerun the same command: successful
files retain their progress, so one unreadable file does not duplicate the readable history.
For long histories, repeated attempts make progress; an operator may also raise `drain_deadline`.
A served deadline outranks the local setting, so change it centrally if the served config pins it.
A hard crash before the buffered fingerprint commit can repeat the most recent uncommitted uploads.

If the background service is flushing, the one-shot command fails with `state: store is locked by
another flush`. Retry between flushes, or stop the background service using the OS service manager,
run the command, and start the service again afterward.

Completion covers currently collectable files. A warning counts remembered shipped objects still
lacking a confirmed resend under this policy, including missing, excluded, oversize, failed and
unattempted sources. Files already forgotten by state pruning/reset cannot be counted. Files no
longer collectable require the archive reseal workflow below and a custodian identity.

**A forced resend replaces the current version only.** Removing a reader does not revoke their
access to older bucket versions. Re-scrubbing does not remove leaked bytes from older versions or
from downstream data already collected, including append-only ingest history. This command is not
remediation for a scrub leak; deletion of those copies is a separate operator task.

Keep the local state to retain progress. `state reset`, state loss, or changing the source spec can
lose progress and cause another forced upload. Existing fingerprint documents remain readable on
upgrade. After a forced resend writes `seal_policy`, downgrading to an older binary makes its
checksum check discard the store: ordinary uploads are re-probed, and forced-resend progress is lost.

A custodian can reseal the history to a recipient added later. Resealing is the organization's
decision; the fleet manager keeps no record of it. Add the recipient to the organization first, so
a file shipped meanwhile is sealed to it, then, with `quesma-unseal` and `$W/enc` as in the README's
step 6:

```sh
"$W/quesma-unseal" reseal -i "$W/name.agekey" -to age1… "$W/enc" "$W/resealed"
rclone copy --update "$W/resealed/" "aws:$B/v1/organization=acme" --include 'install=*/**'
```

Each object whose manifest does not list that recipient lands in `$W/resealed` under its own key, with
the same payload, sealed to the recipients it lists plus the new one. `--update` skips a key a
shipper rewrote after the copy. The upload adds a version and keeps the earlier one. rclone does not
carry the metadata the fleet manager stored with the object, so after a shipper's `state reset`
those files upload once more.

An analytics identity is fixed the same way, and earlier: it can be set only when the organization is
created, so an organization created without one has no history a processor can open later, unless a
custodian reseals it to the processor's recipient. Upgrade
every replica before creating the first one: an older replica cannot read a configuration that has
it, and then lists no organizations at all.

**A re-uploaded file keeps its earlier versions.** The bucket is versioned and nothing expires mirror
objects, so a value that leaked under old scrub rules is still readable in the earlier version until
an operator deletes it by hand. A `state reset` after a shipper upgrade that changed scrubbing
re-uploads every file the new binary scrubs differently.

One thing has no dependency at all: installing the shipper binary. It registers a service that waits
for enrollment, so you can push it out ahead of everything else and enroll later.

## Afterwards

**Rotate the administrator credential.** With Terraform,
`terraform apply -replace=random_bytes.admin_credential`; the digest object changes during the
apply and the old credential stops working immediately. Locally, delete
`fleet-manager/data/admin-credential` and run `make -C fleet-manager run` again.

**Give a third party one organization.** **Partner → Create credential** in the administration UI
(or `POST /v1/admin/orgs/{org}/partner`) shows an `fmp1.` credential once. It can manage that
organization's machines, report their health and edit collection settings
(`PUT /v1/admin/orgs/{org}/collection`, with the ETag from `GET /config`). It cannot change
recipients, Quesma ETL or telemetry, and cannot reach another organization. An organization has one
partner credential, and creating another while it is active answers 409. **Revoke** (or
`POST /v1/admin/orgs/{org}/partner/revoke`) stops it on the next request. To rotate, revoke and
create again, which **Partner → Replace credential** does in one step, then hand the new one over before the
third party's next call. `GET /v1/admin/orgs/{org}/partner` says when the current one was created
and whether it is revoked, and answers 404 when none was ever created. Each partner call that
changes something is logged as `organization <org>: the partner credential's <method> <path>
answered <status>`. Configuration changes also have durable attribution, described below. The
administration UI does not accept a partner credential. Upgrade every replica before creating one: an older replica
refuses it with 401.

**See who changed configuration.** The Configuration view shows whether the current configuration
was written by an **administrator** or **partner**, beside its update time. These identify credential
classes, not individual people sharing a credential. Organization creation, `PUT /config` and
`PUT /collection` are covered. Other partner mutations remain in the service log.

`GET /v1/admin/orgs/{org}/config` returns the same JSON as before, plus a `Config-Updated-By` header
(`administrator`, `partner`, `unknown` or `unavailable`). Attribution is stored separately under
`control/config-authorship/<sha256>.json`; the digest covers the exact stored configuration bytes.
Each immutable record contains `schema`, `author` and `updated_at`, without credentials or a copy of
the configuration. The records are immutable to API callers, the partner included, but someone with
direct bucket access can overwrite them, or write a `config.json` with a matching record. Each
record is created before the conditional configuration write, so a crash cannot commit an
unattributed change. An attribution write failure prevents the change. Interrupted or conflicting
writes may leave unused records; only the record matching the current configuration is displayed.

The view names only the last writer, so an administrator's later save, even of an unchanged form, is
shown as the author of a partner's earlier change. To find who made an earlier change, list the
`config.json` object versions, take the sha256 of each version's bytes, and read
`control/config-authorship/<digest>.json`.

The stored `config.json` format and permissions are unchanged. Older replicas and rollbacks can
still read it, and ignore the new records. Configurations written before this feature or
subsequently by older replicas show **author unknown**. A record that cannot be read or does not
match shows **author unavailable** and is logged; the configuration still loads and can be edited.
Upgrade every replica before relying on attribution, especially before giving a partner that holds a
recipient its credential. Attribution does not identify writers that edit bucket objects directly.

**Revoke.** Installs, invites and grants are revoked from the administration UI. A revoked install
can still be given a name, because the objects it already uploaded remain in the bucket. There is
no organization deletion: retention, credential revocation and key destruction need answers first.

**Change what organizations get by default.** `default_allow_quesma_etl` and
`default_telemetry_collector_url` in the Terraform (`FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL` and
`FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL` for the service) apply to every organization that
never set the value itself, including organizations created before the change. An organization's
own setting always wins, and the administration UI always sets `allow_quesma_etl`: its checkbox
starts from the default, so a later change of the default reaches only organizations created
through the admin API without the field. `deploy.sh` states `default_allow_quesma_etl = false` for a
deployment it creates. The templates' own default stays `true`, so a deployment made before, upgraded
with any copy of `deploy.sh` or by its own Terraform root, keeps what it had.

**Upgrade.** Push a new image and apply. The service is stateless and its records are
conditional-write guarded, so a rolling replacement and a mixed fleet of replicas are both safe.
Shippers update themselves through the signed TUF channel. This is pre-1.0: the wire protocol, the
configuration format and the object naming are pinned by tests but can still change between minor
releases.

**Back up three things,** which fail differently: the private `age` identities (losing them makes
every object ever written permanently unreadable), the bucket (it *is* the database), and the
Terraform state. If you mirror the archive, exclude `private/`. It holds the telemetry signing
seed, which is why it sits outside `v1/`.

**Let an outside ETL read.** An ETL that runs elsewhere, such as Quesma's for its dashboards, reads
the bucket through a bucket policy, so nothing is assumed and no credential is exchanged:

```sh
terraform apply … -var='etl_reader_role_arns=["arn:aws:iam::123456789012:role/etl-reader"]'
```

It permits listing the bucket below `v1/` and reading two kinds of object: everything under an
install's root, and each organization's `control/config.json`. Reading is not decrypting: the ETL
can open objects only while the organization keeps Quesma's recipient (`allow_quesma_etl`, off by
default), or while `analytics_decrypt_role_arns` names it for an organization created with an
analytics identity ([AWS template](terraform/aws/README.md#an-analytics-identity-under-your-own-key)).
Reading and decrypting revoke separately. Empty the list to stop the reading, which takes effect
immediately. Turn off `allow_quesma_etl` to stop the decrypting of objects sealed from then on, or
remove the role from `analytics_decrypt_role_arns` to stop the decrypting of the analytics identity.
That stops new unwraps only: a processor that already unwrapped the identity can keep it, so only
removing its bucket read stops it.

**Running without egress.** Fleet Manager needs no outbound internet access beyond its cloud's own
APIs: object storage, plus IAM Credentials `SignBlob` on Google Cloud, which signs every upload
ticket, and Azure Resource Manager on Azure, which confirms versioning. Its UI assets are embedded
in the binary. What does reach out: shipper
self-update from `updates.quesma.dev` (`SHIPPER_NO_SELFUPDATE` stops it), telemetry forwarding if
you named a collector, and image pulls if you did not build your own.

## Appendix: the bucket by hand

Use this only if you use neither `make run` nor the Terraform templates: an S3-compatible store
other than MinIO, or a policy that requires you to create the bucket yourself.

**What the store must provide.** Object versioning; conditional writes (`If-None-Match: *` and
`If-Match`); presigned PUT with `Content-Length` signed; object tagging on PUT; and user metadata on
PUT. Only `x-amz-tagging` and `x-amz-meta-*` may appear as signed headers on a ticket. Any other
signed header is rejected.

**Key layout.**

```
v1/control/admin/credential.json                     sha256 of the administrator credential
v1/control/reporter/credential.json                  sha256 of the reporter credential (optional)
private/fleet-manager/telemetry-identity.json        the deployment's Ed25519 signing seed
v1/organization=<org>/control/…                      config, grants, invites, install records
v1/organization=<org>/control/partner/credential.json sha256 of the partner credential (optional)
v1/organization=<org>/control/seen/<install>.json    check-in records (tagged ephemeral)
v1/organization=<org>/control/catalogs/<sha256>.json reported source catalogs, written once each
v1/organization=<org>/control/config-authorship/<sha256>.json configuration authorship, written once
v1/organization=<org>/install=<install-id>/tags.json the install's display name
v1/organization=<org>/install=<install-id>/…         the sealed trajectory objects
```

**Create it.**

```sh
aws s3api create-bucket --bucket "$BUCKET" --region "$AWS_REGION" \
  --create-bucket-configuration "LocationConstraint=$AWS_REGION"   # omit this line in us-east-1

aws s3api put-bucket-versioning --bucket "$BUCKET" \
  --versioning-configuration Status=Enabled

aws s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration \
  'BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true'
```

The lifecycle rule that expires superseded check-in records filters on a **tag**, not a prefix,
because the organization sits in the middle of the key and S3 prefixes take no wildcard:

```sh
cat > lifecycle.json <<'JSON'
{"Rules": [{
  "ID": "expire-superseded-ephemeral-records",
  "Status": "Enabled",
  "Filter": {"Tag": {"Key": "lifecycle", "Value": "ephemeral"}},
  "NoncurrentVersionExpiration": {"NoncurrentDays": 1}
}]}
JSON

aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" \
  --lifecycle-configuration file://lifecycle.json
```

**The runtime policy.** No delete anywhere. Below `install=` it writes, tags and reads: the read
serves install names and upload deduplication, which HEADs mirror objects, and S3 authorizes a
HEAD as `s3:GetObject`. The runtime can fetch every sealed payload but holds no age identity to
open one; the listing grant makes an absent object 404 rather than 403. The credential records are
read-only: the service compares a presented credential with the stored digest and never writes one.
Replace `BUCKET` throughout.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ControlObjects",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:PutObjectTagging"],
      "Resource": [
        "arn:aws:s3:::BUCKET/v1/organization=*/control/*",
        "arn:aws:s3:::BUCKET/private/fleet-manager/telemetry-identity.json"
      ]
    },
    {
      "Sid": "CredentialsRead",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": [
        "arn:aws:s3:::BUCKET/v1/control/admin/credential.json",
        "arn:aws:s3:::BUCKET/v1/control/reporter/credential.json"
      ]
    },
    {
      "Sid": "ControlList",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::BUCKET",
      "Condition": {"StringLike": {"s3:prefix": [
        "v1/organization=",
        "v1/organization=*/control/*",
        "v1/control/admin/credential.json",
        "v1/control/reporter/credential.json",
        "private/fleet-manager/telemetry-identity.json"
      ]}}
    },
    {
      "Sid": "InstallAbsence",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::BUCKET",
      "Condition": {"StringLike": {"s3:prefix": "v1/organization=*/install=*"}}
    },
    {
      "Sid": "TrajectoryWriteAndTagOnly",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:PutObjectTagging"],
      "Resource": "arn:aws:s3:::BUCKET/v1/organization=*/install=*"
    },
    {
      "Sid": "InstallRead",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::BUCKET/v1/organization=*/install=*"
    },
    {
      "Sid": "VerifyBucketVersioning",
      "Effect": "Allow",
      "Action": "s3:GetBucketVersioning",
      "Resource": "arn:aws:s3:::BUCKET"
    }
  ]
}
```

`PutObjectTagging` is not optional: without it S3 rejects the whole tagged PUT, not just the tag.
[terraform/aws/main.tf](terraform/aws/main.tf) is the authority for this policy and
`src/terraform_test.go` pins it; if the two disagree, the template is right.

**Seed the administrator credential.** The bucket holds only its SHA-256, so what you mint here is
the only copy that will exist.

```sh
umask 077
printf 'fma1.%s' "$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')" > admin-credential

digest=$(openssl dgst -sha256 -r < admin-credential | cut -d' ' -f1)
printf '{"schema":1,"secret_digest":"%s"}' "$digest" > credential.json

aws s3 cp credential.json "s3://$BUCKET/v1/control/admin/credential.json"
```

The file deliberately has **no trailing newline**: the digest covers the credential exactly as you
will present it, and a stray newline authenticates as a different string.

There is a second, optional credential with the prefix `fmr1.` at
`v1/control/reporter/credential.json`, minted the same way, which may report collection health and
nothing else. A process that reads the archive and reports back can then not also create
organizations or revoke installs.
