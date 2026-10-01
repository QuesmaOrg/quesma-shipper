# Fleet Manager on AWS

This template is what [`deploy.sh aws`](../../deploy.sh) applies in your account. Run it by hand
when you want an image from your own registry, Terraform state kept somewhere other than a
directory on a laptop, or a change to the template. The steps around it, deciding the recipients,
creating the organization and enrolling the shippers, are
[Run Fleet Manager](../../../README.md#operator-set-up-fleet-manager-once) in the repository README.

## What it creates

- A private S3 bucket with versioning on, a full public-access block, and a lifecycle rule that
  expires superseded check-in records the day after they are superseded.
- A runtime role for the service: read and write below every organization's `control/` prefix,
  write and tag below `install=`, read there only to name installs and to deduplicate uploads (it
  can fetch ciphertext, never decrypt it), and read of the two credential digests it verifies
  against. An execution role and an infrastructure role for ECS.
- The administrator and reporter credentials, as random values whose SHA-256 digests are written to
  the bucket. Only the digests leave Terraform; the values are sensitive outputs.
- A 30-day CloudWatch log group and a publicly reachable ECS Express Mode service, which manages
  the Fargate service, load balancer, TLS certificate, networking, autoscaling and monitoring.

`../../src/terraform_test.go` pins what this template may grant.

## Run it by hand

You need an AWS principal permitted to create S3, IAM, CloudWatch Logs and ECS Express Mode
resources; a default VPC in the Region with at least two public subnets in different Availability
Zones and at least eight free addresses in each, which
[ECS Express Mode requires](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-work.html#express-service-network-defaults);
Terraform 1.5 or later, or OpenTofu; and credentials Terraform's AWS provider can use, in
whatever form you already use with the AWS CLI. If you have none yet, the
[AWS CLI authentication guide](https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html)
covers the options.

```sh
export AWS_PROFILE='acme-prod'              # only if your credentials are a named profile
aws sts get-caller-identity                 # the account you are about to change

terraform init
terraform apply -var="region=eu-central-1" -var="bucket=globally-unique-acme-trajectories"

export FLEET_MANAGER_URL="$(terraform output -raw service_url)"
curl --fail --silent --show-error "${FLEET_MANAGER_URL%/}/healthz"     # ok
terraform output -raw admin_url
terraform output -raw admin_credential
```

`tofu` works in place of `terraform` throughout. The committed `.terraform.lock.hcl` pins the
providers as OpenTofu resolves them; Terraform rewrites it with its own registry's entries.

## Variables

| Variable | Default | |
| --- | --- | --- |
| `region` | required | |
| `bucket` | required | S3 bucket names are global |
| `name` | `fleet-manager` | the service and the prefix of its roles |
| `image_uri` | `docker.io/quesma/fleet-manager:latest` | a moving tag Quesma publishes; see below |
| `default_allow_quesma_etl` | `true` | seal uploads to Quesma's recipient for organizations that never chose |
| `default_telemetry_collector_url` | empty | forward shipper telemetry there for organizations that name nowhere |
| `etl_reader_role_arns` | `[]` | who outside the service may read the sealed objects; see below |

## Your own image

The template deploys Quesma's published image unless you name one. Build from a clone and push to
your own registry to control the supply chain, then deploy by digest rather than by tag, so the
service cannot come up on an image you did not build:

```sh
cd fleet-manager
make check                                  # the gate: formatting, vet, licenses, tests

export REGISTRY='123456789012.dkr.ecr.eu-central-1.amazonaws.com'
rev=$(git rev-parse --short=12 HEAD)
aws ecr get-login-password --region eu-central-1 \
  | docker login --username AWS --password-stdin "$REGISTRY"
docker buildx build --platform linux/amd64 --target cloud-run \
  --build-arg VERSION="$(cat VERSION)+$rev" -t "$REGISTRY/fleet-manager:$rev" --push .
docker buildx imagetools inspect "$REGISTRY/fleet-manager:$rev" \
  --format '{{json .Manifest.Digest}}' | tr -d '"'

terraform apply … -var="image_uri=$REGISTRY/fleet-manager@sha256:…"
```

`linux/amd64` is the architecture ECS Express Mode accepts. Without `--build-arg VERSION` the
running service reports `dev`.

## Letting Quesma run the ETL on your data

By default nothing outside this account can read the bucket. The dashboards and the ETL that
produce them run in Quesma's account, so using them means granting its ingest read access to the
sealed objects:

```sh
terraform apply … -var="etl_reader_role_arns=[\"${QUESMA_ETL_ROLE_ARN:?set it to the ARN Quesma supplied}\"]"
```

The grant is a bucket policy, so nothing is assumed and no credential is exchanged. It permits
listing the bucket below `v1/` and reading two kinds of object: everything under an install's root
(the sealed uploads and the install's name) and each organization's `control/config.json`, which
carries its display name. Nothing else: no write, no delete, no bucket configuration, and none of
the other control records. A Deny on reads backs this up, so a reader in the same account whose own
role allows more still reads only these.

Reading is not decrypting. Every object is sealed to your organization's `age` recipients, and
Quesma can open one only while the organization keeps its built-in recipient, the
`allow_quesma_etl` setting. The two are separately revocable, and revoking either is enough:

| to stop | do |
| --- | --- |
| Quesma reading new and existing objects | apply with `etl_reader_role_arns=[]` |
| Quesma decrypting objects sealed from now on | turn off `allow_quesma_etl` for the organization |

Turning off the setting does not re-seal what was already written, which is why removing the
bucket grant is the one that takes effect immediately. Uploads are sealed to Quesma's recipient
whether or not the grant exists, so granting the list later opens your history to Quesma's
analytics, not only what comes after.

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
