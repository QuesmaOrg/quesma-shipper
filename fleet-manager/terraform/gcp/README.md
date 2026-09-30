# Deploy fleet manager on Google Cloud

This guide deploys one multi-tenant fleet manager and its storage bucket, creates an
organization, and enrolls the first shipper. Run the commands from this directory. It is the Google
Cloud counterpart of [Run it in your own cloud](../../../README.md#run-it-in-your-own-cloud), which
sets out what to decide first and the whole sequence around it.

## Before you begin

You need:

- a Google Cloud project with billing enabled;
- permission to enable APIs and create Cloud Run, Cloud Storage, service account,
  IAM role, and IAM policy resources in that project;
- [Terraform](https://developer.hashicorp.com/terraform/install) 1.5 or later,
  [`age`](https://age-encryption.org/) (for `age-keygen`), and `curl`; and
- an organization policy that permits
  [public invocation](https://cloud.google.com/run/docs/authenticating/public) of
  the Cloud Run service; shippers authenticate at the application layer after
  enrollment.

## 1. Authenticate and select the deployment

Authenticate to Google Cloud using a method supported by Terraform. Your credentials must be
allowed to create the resources listed in [Before you begin](#before-you-begin). If you have not
configured credentials yet, follow [Google Cloud's Terraform authentication guide](https://cloud.google.com/docs/terraform/authentication).

Set the project and deployment Region:

```sh
export PROJECT_ID='your-gcp-project-id'
export REGION='your-gcp-region-of-choice' # e.g. europe-central2
```

Confirm that `$PROJECT_ID` is the project where you intend to deploy. Terraform receives both
values explicitly below and uses the credentials available to its Google Cloud provider.

## 2. Deploy

```sh
terraform init
terraform apply \
  -var="project=$PROJECT_ID" \
  -var="region=$REGION"
```

The service uses Quesma's published `docker.io/quesma/fleet-manager:latest` image by default.

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
```

The command saves the private identity to `acme-security.agekey` and prints its public recipient.
Never place a private identity in this repository, Terraform variables, or Terraform state.

1. Retrieve the sensitive administrator credential:

   ```sh
   terraform output -raw admin_credential
   ```

2. Get the administration URL and open it in a browser:

   ```sh
   terraform output -raw admin_url
   ```

3. Paste the credential into the login form and sign in. Create an organization with an immutable
   lowercase slug, a display name, and at least two independently held public recipients. The
   optional authored YAML field controls that organization's collection settings. Use the selector
   to create or switch organizations.

## 4. Install and enroll the first shipper

Create one invite per machine in the administration UI. The invite can be used
once, and its secret is shown only when created. Send it and `FLEET_MANAGER_URL` to the
machine operator through an approved secret-sharing channel.

On the target machine, install the shipper for its platform as in
[Install](../../../README.md#1-install), then enroll it. This is the command the administration UI
shows next to a new invite:

```sh
export SHIPPER_AUTH_KEY='fmi2.acme.invite-from-the-fleet-operator'
export FLEET_MANAGER_URL='https://fleet-manager-service-url'

quesma-shipper login --server "$FLEET_MANAGER_URL" "$SHIPPER_AUTH_KEY"
unset SHIPPER_AUTH_KEY

quesma-shipper doctor
```

Release builds of the shipper keep themselves current from Quesma's signed update channel, a
[TUF](https://theupdateframework.io/) repository; see the [shipper README](../../../README.md).

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
