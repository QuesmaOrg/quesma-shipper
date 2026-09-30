# Fleet Manager on Google Cloud

This template is what [`deploy.sh gcp`](../../deploy.sh) applies in your project. Run it by hand
when you want an image from your own registry, Terraform state kept somewhere other than a
directory on a laptop, or a change to the template. The steps around it, deciding the recipients,
creating the organization and enrolling the shippers, are
[Run Fleet Manager](../../../README.md#operator-set-up-fleet-manager-once) in the repository README.

## What it creates

- The IAM, IAM Credentials, Cloud Run and Cloud Storage APIs, enabled in the project.
- A Cloud Storage bucket with versioning on, uniform bucket-level access and public-access
  prevention enforced. GCS has no tag-based lifecycle condition, so superseded check-in records
  accumulate; expire them with an out-of-band job if that matters for the fleet's size.
- A runtime service account with custom roles bound by condition: read, write and delete below
  every organization's `control/` prefix; create, delete and read below `install=` (read only to
  name installs and to deduplicate uploads; it can fetch ciphertext, never decrypt it); read of the
  two credential digests; and `signBlob` on itself, because GCS presigned URLs are signed through
  the IAM API. Delete is included because GCS requires delete-plus-create to replace object bytes;
  versioning preserves the displaced version.
- The administrator and reporter credentials, as random values whose SHA-256 digests are written to
  the bucket. Only the digests leave Terraform; the values are sensitive outputs.
- A publicly invocable Cloud Run service. Shippers authenticate at the application layer after
  enrollment, so the organization policy must permit
  [public invocation](https://cloud.google.com/run/docs/authenticating/public).

`../../src/terraform_test.go` pins what this template may grant.

## Run it by hand

You need a project with billing enabled and permission to enable APIs and create Cloud Run, Cloud
Storage, service account, IAM role and IAM policy resources in it; Terraform 1.5 or later, or
OpenTofu; and credentials Terraform's Google provider can use, for example Application Default
Credentials from `gcloud auth application-default login`. If you have none yet,
[Google Cloud's Terraform authentication guide](https://cloud.google.com/docs/terraform/authentication)
covers the options. The `gcloud` CLI itself is needed only by `deploy.sh`, for its preflight.

```sh
export PROJECT_ID='acme-prod'               # the project you are about to change

terraform init
terraform apply -var="project=$PROJECT_ID" -var="region=europe-central2"

export FLEET_MANAGER_URL="$(terraform output -raw service_url)"
curl --fail --silent --show-error "${FLEET_MANAGER_URL%/}/health"     # ok; Cloud Run reserves /healthz
terraform output -raw admin_url
terraform output -raw admin_credential
```

`tofu` works in place of `terraform` throughout. The committed `.terraform.lock.hcl` pins the
providers as OpenTofu resolves them; Terraform rewrites it with its own registry's entries.

## Variables

| Variable | Default | |
| --- | --- | --- |
| `project` | required | |
| `region` | `europe-central2` | |
| `name` | `fleet-manager` | the service, its service account and the prefix of its roles |
| `bucket` | `<project>-trajectories` | |
| `image` | `docker.io/quesma/fleet-manager:latest` | a moving tag Quesma publishes; see below |
| `default_allow_quesma_etl` | `true` | seal uploads to Quesma's recipient for organizations that never chose |
| `default_telemetry_collector_url` | empty | forward shipper telemetry there for organizations that name nowhere |

There is no ETL reader grant here yet: nothing outside the project can read the bucket unless you
grant it yourself.

## Your own image

The template deploys Quesma's published image unless you name one. Build from a clone and push to
a registry Cloud Run can pull from, then deploy by digest rather than by tag:

```sh
cd fleet-manager
make check                                  # the gate: formatting, vet, licenses, tests

export REGISTRY='europe-central2-docker.pkg.dev/acme-prod/images'
rev=$(git rev-parse --short=12 HEAD)
gcloud auth configure-docker europe-central2-docker.pkg.dev
docker buildx build --platform linux/amd64 --target cloud-run \
  --build-arg VERSION="$(cat VERSION)+$rev" -t "$REGISTRY/fleet-manager:$rev" --push .
docker buildx imagetools inspect "$REGISTRY/fleet-manager:$rev" \
  --format '{{json .Manifest.Digest}}' | tr -d '"'

terraform apply … -var="image=$REGISTRY/fleet-manager@sha256:…"
```

`linux/amd64` is the architecture Cloud Run accepts. Without `--build-arg VERSION` the running
service reports `dev`.

## Operations

Rotate the administrator credential by replacing its random resource and applying; the digest
object changes during the apply, immediately invalidating the old credential:

```sh
terraform apply -replace=random_bytes.admin_credential
terraform output -raw admin_credential
```

Protect that credential, the Terraform state, and the private `age` identities according to your
organization's backup and access-control requirements. Do not run `terraform destroy` after
enrollment without a reviewed data-retention plan: the bucket contains fleet state and encrypted
trajectory data, and a non-empty bucket prevents normal destruction. The rest is in
[OPERATIONS.md](../../OPERATIONS.md).
