# Deploy fleet manager on AWS

This guide deploys one multi-tenant fleet manager and its S3 bucket, creates an
organization, and enrolls the first shipper. Run the commands from this directory.

## Before you begin

You need:

- an AWS account and principal permitted to create S3, IAM, CloudWatch Logs, and
  ECS Express Mode resources;
- a default VPC in the target Region with
  [at least two public subnets](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-work.html)
  in different Availability Zones and at least eight free IP addresses in each;
- [Terraform](https://developer.hashicorp.com/terraform/install) 1.5 or later,
  the [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
  with a profile configured for that account (step 1 says how if you have none),
  `age-keygen`, and `curl`; and
- two public `age` recipients held by separate custodians. Never place either
  private identity in this repository, Terraform variables, or Terraform state.

## 1. Authenticate and select the deployment

`AWS_PROFILE` below names a profile that must already exist in `~/.aws/config`. If you have none
for this account, make one first. With IAM Identity Center, once:

```sh
aws configure sso --profile acme-dev
```

It asks for your organisation's access portal URL and the region Identity Center runs in, opens a
browser, then lets you pick the account and the permission set. Answer the default client Region
with the Region you are deploying into, which is a separate setting from the Identity Center one.
With long-lived access keys instead, `aws configure --profile acme-dev` asks for the key, the
secret and the Region.

Set these deployment values. The S3 bucket name must be globally unique.

```sh
export AWS_PROFILE='your-aws-profile'
export AWS_REGION='eu-central-1'
export TRAJECTORIES_BUCKET='globally-unique-acme-trajectories'
```

If the profile uses IAM Identity Center, authenticate before continuing. A session lasts hours,
so this is the one command you repeat; `aws configure sso` is not:

```sh
aws sso login --profile "$AWS_PROFILE"
```

Terraform uses the standard AWS credential chain. Confirm
the selected account and Region:

```sh
aws sts get-caller-identity
aws configure get region --profile "$AWS_PROFILE"
```

If the second command is empty or differs from `AWS_REGION`, Terraform still uses
the explicit `region` value below.

## 2. Deploy

```sh
terraform init
terraform apply \
  -var="region=$AWS_REGION" \
  -var="bucket=$TRAJECTORIES_BUCKET"
```

`tofu` works in place of `terraform` throughout. The committed `.terraform.lock.hcl` pins the
providers as OpenTofu resolves them (`registry.opentofu.org`). Terraform resolves them from
`registry.terraform.io` instead, so `terraform init` rewrites that file with its own entries.

Terraform creates a private, versioned S3 bucket; scoped runtime and execution
roles; a 30-day CloudWatch log group; and a publicly reachable ECS Express Mode
service. Express Mode manages the Fargate service, load balancer, TLS certificate,
networking, autoscaling, and monitoring.

Record the output and verify the service:

```sh
export FLEET_MANAGER_URL="$(terraform output -raw service_url)"
terraform output bucket
curl --fail --silent --show-error "${FLEET_MANAGER_URL%/}/healthz"
```

The health request must return `ok`.

## 3. Create the organization

Ask each custodian to generate a private identity outside fleet manager and send
you only its printed `age1...` public recipient:

```sh
age-keygen -o acme-security.agekey
age-keygen -y acme-security.agekey
```

Retrieve the sensitive administrator credential explicitly, open the administration
URL, and paste the credential into the login form:

```sh
terraform output -raw admin_url
terraform output -raw admin_credential
```

Create an organization with an immutable lowercase slug, a display name, and at least two
independently held public recipients. The optional authored YAML field controls that
organization's collection settings. Use the selector to create or switch organizations.

## 4. Enroll the first shipper

Create one invite per machine in the administration UI. The invite can be used
once, and its secret is shown only when created. Send it and `FLEET_MANAGER_URL` to the
machine operator through an approved secret-sharing channel. On the target Linux
or macOS machine, run:

```sh
export SHIPPER_AUTH_KEY='fmi2.acme.invite-from-the-fleet-operator'
export FLEET_MANAGER_URL='https://fleet-manager-service-url'

curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/src/packaging/linux/install.sh \
  | sh -s -- --server "$FLEET_MANAGER_URL"
unset SHIPPER_AUTH_KEY

"$HOME/.local/bin/quesma-shipper" doctor
```

The installer verifies the bootstrap checksum, enrolls the machine, and starts the
per-user background service. It must not be run as root. Release builds of the shipper
keep themselves current from Quesma's signed update channel, a
[TUF](https://theupdateframework.io/) repository; see the
[shipper README](https://github.com/QuesmaOrg/quesma-shipper#readme).

Confirm the enrollment on the UI's Installs page. The new install must appear
with status `active`. Repeat this section with a new
invite for each additional machine.

## Letting Quesma run the ETL on your data

By default nothing outside this account can read the bucket. The dashboards and the ETL that
produce them run in Quesma's account, so using them means granting its ingest read access to the
sealed objects:

```sh
terraform apply \
  -var="region=$AWS_REGION" \
  -var="bucket=$TRAJECTORIES_BUCKET" \
  -var='etl_reader_role_arns=["arn:aws:iam::123456789012:role/etl-reader"]'
```

Quesma supplies the ARN; it is the role its ingest job runs as. The
grant is a bucket policy, so nothing is assumed and no credential is exchanged. It permits listing
the bucket below `v1/` and reading two kinds of object: everything under an install's root
(`v1/organization=<org>/install=<id>/`, the sealed uploads and the install's name) and each
organization's `control/config.json`, which carries its display name. Nothing else: no write, no
delete, no bucket configuration, and none of the other control records -- invites, grants, install
records and credential digests. A Deny backs this up, so a reader whose own role allows more
(one in the same account as the bucket, say) still gets only these.

Reading is not decrypting. Every object is sealed to your organization's `age` recipients, and
Quesma can open one only while the organization keeps its built-in recipient — the
`allow_quesma_etl` setting in the administration UI. It is on unless you turn it off for the
organization, or set `default_allow_quesma_etl = false` for every organization that never chose;
set that before the first upload if nothing should ever be sealed to Quesma. The two are separately
revocable, and revoking either is enough:

| to stop | do |
| --- | --- |
| Quesma reading new and existing objects | apply with `etl_reader_role_arns=[]` |
| Quesma decrypting objects sealed from now on | turn off `allow_quesma_etl` for the organization |

Turning off the setting does not re-seal what was already written. Objects sealed while it was on
stay readable to the recipient they were sealed to, which is why removing the bucket grant is the
one that takes effect immediately.

Leaving the list empty keeps everything here, which is also what you get by changing nothing: the
shipper, this service, the bucket and the trajectories never leave your account, and Quesma's
dashboards are unavailable. Uploads are still sealed to Quesma's recipient too, so granting the
list later opens your history to Quesma's analytics, not only what comes after.

## Operations

Use the administration UI to update configuration; create, list, and revoke grants
or invites; release recoverable invite reservations; and list or revoke installs.

Rotate the administrator credential by replacing its random resource and applying:

```sh
terraform apply -replace=random_bytes.admin_credential
```

The digest object changes during the apply, immediately invalidating the old credential. Retrieve
the replacement with `terraform output -raw admin_credential`. Protect that credential, Terraform
state, and the two private `age` identities according to your organization's backup and
access-control requirements. Do not run
`terraform destroy` after enrollment without a reviewed data-retention plan; the
bucket contains fleet state and encrypted trajectory data, and a non-empty bucket
prevents normal destruction.

## Telemetry public key

Register the deployment's telemetry signing key with the collector (see
[TELEMETRY.md](../../TELEMETRY.md)):

```sh
terraform output -raw fleet_manager_public_key
```
