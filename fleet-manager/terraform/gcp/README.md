# Deploy fleet manager on Google Cloud

This guide deploys one multi-tenant fleet manager and its storage bucket, creates an
organization, and enrolls the first shipper. Run the commands from this directory.

## Before you begin

You need:

- a Google Cloud project with billing enabled;
- permission to enable APIs and create Cloud Run, Cloud Storage, service account,
  IAM role, and IAM policy resources in that project;
- [Terraform](https://developer.hashicorp.com/terraform/install) 1.5 or later,
  the [`gcloud` CLI](https://cloud.google.com/sdk/docs/install), `age-keygen`, and `curl`;
- an organization policy that permits
  [public invocation](https://cloud.google.com/run/docs/authenticating/public) of
  the Cloud Run service; shippers authenticate at the application layer after
  enrollment; and
- two public `age` recipients held by separate custodians. Never place either
  private identity in this repository, Terraform variables, or Terraform state.

## 1. Authenticate and select the deployment

Set these deployment values.

```sh
export PROJECT_ID='your-gcp-project-id'
export REGION='europe-central2'

gcloud auth login
gcloud auth application-default login
gcloud config set project "$PROJECT_ID"
```

Terraform uses Application Default Credentials. Confirm
that the selected account and project are correct before continuing:

```sh
gcloud auth list --filter=status:ACTIVE
gcloud config get-value project
```

## 2. Deploy

```sh
terraform init
terraform apply \
  -var="project=$PROJECT_ID" \
  -var="region=$REGION"
```

`tofu` works in place of `terraform` throughout. The committed `.terraform.lock.hcl` pins the
providers as OpenTofu resolves them (`registry.opentofu.org`). Terraform resolves them from
`registry.terraform.io` instead, so `terraform init` rewrites that file with its own entries.

Unless overridden with `-var='bucket=...'`, the bucket is named
`<project-id>-trajectories`. Terraform enables object versioning and public-access
prevention on the bucket, creates the runtime service account and scoped IAM
roles, and deploys a publicly reachable Cloud Run service.

Record the outputs and verify the service:

```sh
export FLEET_MANAGER_URL="$(terraform output -raw service_url)"
terraform output bucket
curl --fail --silent --show-error "${FLEET_MANAGER_URL%/}/health"
```

The health request must return `ok`. Cloud Run reserves `/healthz`; this deployment
uses the equivalent `/health` endpoint.

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
