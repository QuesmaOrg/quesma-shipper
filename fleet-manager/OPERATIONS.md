# Operating Fleet Manager

What to know once Fleet Manager is running: why the installation order matters, the day-to-day
operations, and how to prepare storage by hand when you use neither `make run` nor the Terraform
templates. The installation itself is in the repository [README](../README.md#get-started).

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

**The age recipients are fixed when the organization is created.** Files are encrypted to the
recipients registered at the time of upload. Changing the recipients later does not re-encrypt
existing files.

One thing has no dependency at all: installing the shipper binary. It registers a service that waits
for enrollment, so you can push it out ahead of everything else and enroll later.

## Afterwards

**Rotate the administrator credential.** With Terraform,
`terraform apply -replace=random_bytes.admin_credential`; the digest object changes during the
apply and the old credential stops working immediately. Locally, delete
`fleet-manager/data/admin-credential` and run `make -C fleet-manager run` again.

**Revoke.** Installs, invites and grants are revoked from the administration UI. A revoked install
can still be given a name, because the objects it already uploaded remain in the bucket. There is
no organization deletion: retention, credential revocation and key destruction need answers first.

**Change what organizations get by default.** `default_allow_quesma_etl` and
`default_telemetry_collector_url` in the Terraform (`FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL` and
`FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL` for the service) apply to every organization that
never set the value itself, including organizations created before the change. An organization's
own setting always wins.

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
can open objects only while the organization keeps Quesma's recipient (`allow_quesma_etl`, on by
default). The two revoke separately. Empty the list to stop the reading, which takes effect
immediately; turn off `allow_quesma_etl` to stop the decrypting of objects sealed from then on.

**Running without egress.** Fleet Manager needs no outbound internet access. It connects to object
storage only, and its UI assets are embedded in the binary. What does reach out: shipper
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
v1/organization=<org>/control/seen/<install>.json    check-in records (tagged ephemeral)
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

**The runtime policy.** Almost write-only below `install=`: no delete anywhere, and no read of a
payload. The one read below `install=` is `tags.json`, granted as that single object name rather
than the prefix, which would be read access to every sealed payload. The credential records are
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
      "Sid": "InstallNamesAbsence",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::BUCKET",
      "Condition": {"StringLike": {"s3:prefix": "v1/organization=*/install=*/tags.json"}}
    },
    {
      "Sid": "TrajectoryWriteAndTagOnly",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:PutObjectTagging"],
      "Resource": "arn:aws:s3:::BUCKET/v1/organization=*/install=*"
    },
    {
      "Sid": "InstallNamesRead",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::BUCKET/v1/organization=*/install=*/tags.json"
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
