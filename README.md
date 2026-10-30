# Quesma Shipper

[![version](https://img.shields.io/badge/version-0.0.3-blue)](#developer-install-the-shipper-each-machine)

Quesma Shipper collects the session files that AI coding agents write on developer machines,
redacts the secrets and personal data its rules detect in transcripts and context files, encrypts
everything, and uploads the ciphertext to object storage your organisation controls. It is two
parts: **the shipper** ([`src/`](src/README.md)), a background service on each developer machine,
and **[Fleet Manager](fleet-manager/)**, the control plane you run in your own cloud, which enrolls
shippers and signs each upload but never sees file contents.

```mermaid
flowchart LR
    S["quesma-shipper<br/>scrub → encrypt (age)"]
    F["Fleet Manager<br/>stateless"]
    B[("your object store")]

    S -->|enroll| F
    F -.->|"config, upload tickets"| S
    F -.->|"control records"| B
    S ==>|"ciphertext, direct to storage"| B
```

Each upload is sealed to the organisation's [age](https://age-encryption.org/) recipients, and any
one of their private keys can decrypt it: your custodians' and, unless you turn Quesma ETL off,
Quesma's. Account and usage records are encrypted but not scrubbed.

Supported agents: Claude Code, Codex, Cursor, GitHub Copilot (the Copilot CLI and Copilot Chat in
VS Code), Pi\*, OpenCode\*, Hermes\*. Supported platforms: macOS 13 or newer, Linux, Windows 10
1809 or newer. The project is pre-1.0: the wire protocol, the configuration format and the object
naming are pinned by tests but can still change between minor releases.

\* Experimental. We actively run and test Claude Code, Codex, and Cursor.
Pi, OpenCode, and Hermes support may change or be withdrawn.

## Get started

| | |
| --- | --- |
| **Try&nbsp;it&nbsp;first** | [**WALKTHROUGH.md**](fleet-manager/WALKTHROUGH.md): every step below on a throwaway deployment, a container as the developer machine, torn down at the end. About fifteen minutes. |
| **Set&nbsp;it&nbsp;up** | [Operator: set up Fleet Manager](#operator-set-up-fleet-manager-once), once, in your AWS account or Google Cloud project. |
| **Join&nbsp;it** | [Developer: install the shipper](#developer-install-the-shipper-each-machine), on each machine. |

## Operator: set up Fleet Manager (once)

To run a self-managed Quesma Shipper fleet that sends data to a bucket you own, first deploy
Fleet Manager in your cloud account. Fleet Manager is the shared service that enrolls developer
machines, supplies their collection settings and encryption recipients, and authorizes uploads.

The steps below walk you through a reference setup on **AWS ECS Express Mode with an S3 bucket**
or **Google Cloud Run with a Cloud Storage (GCS) bucket**. The deployment script creates Fleet
Manager, the bucket, and the cloud permissions they need. You then use its admin UI to create
your organisation and invite developers to join. Set this up once for your fleet; developers
can then follow [the shipper installation steps](#developer-install-the-shipper-each-machine)
on each machine.

1. **Have ready:** an AWS account or Google Cloud project you can create resources in, and on your
   laptop Terraform or OpenTofu, `curl`, `age`, `age-keygen`, `zstd`, and the `aws` CLI signed in,
   or `gcloud` with `gcloud auth application-default login`. On AWS the Region needs a default VPC.
   On Google Cloud, make sure the project is set up with billing before you deploy: the script
   deploys into an existing project and never creates one. If you are unsure how, or which of the
   identifiers Google shows you is the one to pass, follow the
   [addendum](#addendum-set-up-a-google-cloud-project-with-billing) at the bottom of this page.

2. **Decide two things** before anything enrolls. Ask two custodians to each run `age-keygen -o
   name.agekey` and send you only their `age1…` public recipient (`age-keygen -y name.agekey`
   prints it); their private identities are your only way to read what is uploaded, so they
   must be kept and backed up. Decide whether Quesma may decrypt uploads, for its dashboards; it is
   on unless you say `--no-quesma-etl` below, and cannot be added to files already uploaded.

3. **Deploy:**

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/fleet-manager/deploy.sh

   sh deploy.sh aws --bucket globally-unique-acme-trajectories --region eu-central-1   # AWS, or
   sh deploy.sh gcp --project acme-prod --region europe-central2                        # Google Cloud
   ```

   It shows the plan, asks for `yes`, and after a few minutes prints the service URL, the admin UI
   address and the admin credential. Keep `~/.quesma/fleet-manager/<cloud>/` backed up, since the
   Terraform state is there. Running the same command again applies the latest template; upgrading
   the service image is in [OPERATIONS.md](fleet-manager/OPERATIONS.md). `sh deploy.sh aws output
   admin_credential` prints the credential again; `sh deploy.sh --help` lists the rest.

4. **Open the admin UI**, paste the credential, and create the organisation: a slug such as `acme`
   (permanent, it is part of every object key), a display name, the two recipients, the Quesma
   checkbox. Verify a custodian can decrypt a test file sealed to those recipients:

   ```sh
   echo test | age -r age1… -r age1… -o test.age     # the two recipients you registered
   age -d -i name.agekey test.age                    # a custodian, with their private identity
   ```

5. **On the Invites page**, create one invite per developer; its `fmi2.…` token is shown once.
   Send the token and the service URL to each developer through a secret-sharing channel. Each OS
   user on a machine enrolls separately, as its own install. A grant, on the Grants page, enrolls
   any number of installs until it expires or is revoked, which suits shared machines and MDM
   rollouts.

6. **Later, check a session arrived** and decrypt it with a custodian key. `active` on the
   Installs page means a machine enrolled, not that it has shipped anything.

   ```sh
   B=$(sh deploy.sh aws output bucket)
   KEY=$(aws s3api list-objects-v2 --bucket "$B" --prefix 'v1/organization=acme/install=' \
     --query "Contents[?contains(Key, 'claude-code-transcripts')].Key | [0]" --output text)
   aws s3 cp "s3://$B/$KEY" object.age
   age -d -i name.agekey object.age | zstd -d | tar -t     # manifest.json, payload
   ```

   On Google Cloud, `gcloud storage ls --recursive` on the bucket lists the same keys.

What the script creates, and running the template by hand with your own image or your own
state: [fleet-manager/terraform/aws](fleet-manager/terraform/aws/README.md),
[fleet-manager/terraform/gcp](fleet-manager/terraform/gcp/README.md). Rotating the credential,
revoking installs, upgrading, backups and letting an outside ETL read the bucket:
[fleet-manager/OPERATIONS.md](fleet-manager/OPERATIONS.md). Everything on one laptop instead, with
no cloud account: [On one machine](fleet-manager/README.md#on-one-machine).

## Developer: install the shipper (each machine)

1. **Install.** Personal installations need no administrator rights. The macOS package and
   Windows setup also offer an administrator-managed all-users installation.
   - macOS: `brew install --cask quesmaorg/tap/quesma-shipper`, or the signed
     [quesma-shipper-macos-universal.pkg](https://updates.quesma.dev/download/quesma-shipper-macos-universal.pkg)
   - Linux: `curl -fsSLO https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/src/packaging/linux/install.sh && sh install.sh`
   - Windows: run [QuesmaShipperSetup-amd64.exe](https://updates.quesma.dev/download/QuesmaShipperSetup-amd64.exe)
     (x64) or [QuesmaShipperSetup-arm64.exe](https://updates.quesma.dev/download/QuesmaShipperSetup-arm64.exe) (Arm)

2. **Enroll** with the token and URL the operator sent:

   ```sh
   quesma-shipper login <token> --server https://fleet-manager-url
   ```

3. **Run `quesma-shipper doctor`.** It should say it is sending to your organisation. The service
   ships on start and then every 15 minutes, and keeps itself up to date.

Uninstalling, MDM and Intune deployment, what is collected, and how to pause or configure it:
[src/README.md](src/README.md).

## Learn more

[fleet-manager/WALKTHROUGH.md](fleet-manager/WALKTHROUGH.md) rehearses everything above on a
throwaway deployment; [fleet-manager/README.md](fleet-manager/README.md) covers the control plane;
[CONSTITUTION.md](CONSTITUTION.md) the design rules; [ARCHITECTURE.md](ARCHITECTURE.md) the code
layout; [CONTRIBUTING.md](CONTRIBUTING.md) building, testing and releases; [SECURITY.md](SECURITY.md)
reporting a vulnerability. The wire contract is the [shipper-protocol/](shipper-protocol/) module.
Do not attach real
trajectories, agent databases, logs or credentials to issues or pull requests.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). `quesma-shipper licenses` prints the
license and every bundled dependency's license text; Fleet Manager keeps its own in
[fleet-manager/NOTICE](fleet-manager/NOTICE) and `fleet-manager/third_party/`. Quesma, Quesma
Shipper, Quesma Fleet Manager, and the Quesma logo are trademarks of Quesma Inc.; the license does
not grant trademark rights. If you distribute a modified build, read [TRADEMARKS.md](TRADEMARKS.md)
before you name it.

## Addendum: set up a Google Cloud project with billing

`deploy.sh gcp` deploys into a project you already have. It never creates a project and never
touches billing. Before step 3 of the operator section you need a project, an open billing account,
and the two linked. Google shows several identifiers for each, and only one of each is the one to
pass. Check what you have against this table before you type anything.

| Identifier | Looks like | Where you see it | Used here for |
| --- | --- | --- | --- |
| Project ID | `acme-fleet-manager`: 6 to 30 lowercase letters, digits and hyphens, starts with a letter, unique across all of Google Cloud, cannot change | `PROJECT_ID` column of `gcloud projects list`; in the console's project picker, the small text under the name | `--project` in step 3 |
| Project name | `Acme Fleet Manager`: free text with spaces and capitals, can change, not unique | `NAME` column; the large text in the console | nothing; the script does not accept it |
| Project number | `123456789012`: digits only | `PROJECT_NUMBER` column | nothing |
| Billing account ID | `0A1B2C-3D4E5F-6A7B8C`: three groups of six hex characters joined by hyphens | `ACCOUNT_ID` column of `gcloud billing accounts list`; on the console's Billing page, the small text under the account name | `--billing-account` below |
| Billing account name | `Acme Billing`: free text | `NAME` column | nothing |

**Two logins.** `gcloud` keeps two separate credentials. `gcloud auth login` is for the `gcloud`
commands below. `gcloud auth application-default login` is what Terraform, and so `deploy.sh`,
uses. Step 1 asks only for the second; this addendum needs the first too. Use the same Google
account for both.

```sh
gcloud auth login
gcloud auth application-default login
```

**1. A project.** If you already have one, find its ID:

```sh
gcloud projects list
```

```
PROJECT_ID          NAME                PROJECT_NUMBER
acme-fleet-manager  Acme Fleet Manager  123456789012
```

Otherwise create one. Choose the ID carefully, it is permanent; the name is only display text.
With a Google Workspace account the project usually has to sit under your organization:
`gcloud organizations list` prints its numeric ID, and `--organization` places the project there.

```sh
gcloud projects create acme-fleet-manager --name="Acme Fleet Manager" --organization 123456789012
```

A permission error here means your account lacks the Project Creator role in the organization.
Ask an organization administrator for the role, or for a project.

**2. An open billing account.** Nothing in Cloud Run or Cloud Storage works without one.

```sh
gcloud billing accounts list
```

```
ACCOUNT_ID            NAME          OPEN   MASTER_ACCOUNT_ID
0A1B2C-3D4E5F-6A7B8C  Acme Billing  True
```

`OPEN` must be `True`. An empty list, or only `False` rows, means you have no billing account
you may use. Ask whoever administers billing for your organization to grant your account the
Billing Account User role on theirs, or to link it to your project for you. Do not open a
personal billing account to get past this on a company project.

**3. Link them, then verify.**

```sh
gcloud billing projects link acme-fleet-manager --billing-account 0A1B2C-3D4E5F-6A7B8C
gcloud billing projects describe acme-fleet-manager
```

```
billingAccountName: billingAccounts/0A1B2C-3D4E5F-6A7B8C
billingEnabled: true
name: projects/acme-fleet-manager/billingInfo
projectId: acme-fleet-manager
```

`billingEnabled: true` is the line that matters. Now step 3 of the operator section, with the
project ID:

```sh
sh deploy.sh gcp --project acme-fleet-manager --region europe-central2
```

The script checks the project and its billing again before it changes anything, and prints the
account it deploys as. A permission error from Terraform after that point means that account
lacks a role on the project; Owner or Editor on a project you created yourself is enough.
