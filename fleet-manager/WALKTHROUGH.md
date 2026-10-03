# Walkthrough: the whole system on a throwaway deployment

Every step of the repository [README](../README.md), run end to end in about fifteen minutes and
torn down at the end: Fleet Manager deployed into your AWS account, an organization created, a
developer machine enrolled, an upload verified and decrypted. Nothing here touches anything you
already run, and nothing survives step 8.

The developer machine is a Docker container, so the developer's steps run on the same laptop as
the operator's. On a real machine those two steps are just [Install the
shipper](../README.md#developer-install-the-shipper-each-machine).

You need: an AWS account you can create resources in, and on your laptop the `aws` CLI,
Terraform or OpenTofu, `curl`, `age`, `zstd` and Docker. Google Cloud follows the same shape with
`deploy.sh gcp`; the storage commands in steps 6 and 8 become `gcloud storage`.

## 0. A clean directory and a session

```sh
mkdir -p ~/fm-trial && cd ~/fm-trial
export AWS_PROFILE=your-profile
aws sso login --profile "$AWS_PROFILE"      # with IAM Identity Center; `aws configure` otherwise
aws sts get-caller-identity                 # the account you are about to change
```

## 1. Deploy Fleet Manager

About five minutes, most of it the service reaching steady state. `--dir .` keeps the Terraform
state here instead of `~/.quesma/fleet-manager`, so removing this directory at the end removes
everything; `--name` keeps the roles and the service apart from a real deployment in the same
account.

```sh
curl -fsSLO https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/fleet-manager/deploy.sh

sh deploy.sh aws --dir . --name fm-trial --region eu-central-1 \
  --bucket "trial-$(date +%Y%m%d)-$(whoami)-trajectories"
```

Answer `yes` to the plan. It prints the service URL, the admin UI address and the administrator
credential. From now on `sh deploy.sh aws output <name> --dir .` returns any of them.

## 2. Create two custodian keys

Two, because that is the minimum an organization accepts, and held by separate people in
practice. Here both are yours.

```sh
age-keygen -o custodian-1.agekey
age-keygen -o custodian-2.agekey
```

## 3. The organization and an invite

The admin UI does this with a form; the same two calls against the admin API do it from a
terminal, and are how you would automate onboarding.

```sh
URL=$(sh deploy.sh aws output service_url --dir .)
CRED=$(sh deploy.sh aws output admin_credential --dir .)
R1=$(age-keygen -y custodian-1.agekey); R2=$(age-keygen -y custodian-2.agekey)

curl -sS -X POST "$URL/v1/admin/orgs" -H "Authorization: Bearer $CRED" -H 'Content-Type: application/json' \
  -d "{\"slug\":\"acme\",\"display_name\":\"Acme trial\",\"age_recipients\":[\"$R1\",\"$R2\"],\"include_install_recipient\":false,\"allow_quesma_etl\":false,\"collection\":{}}"

INVITE=$(curl -sS -X POST "$URL/v1/admin/orgs/acme/invites" -H "Authorization: Bearer $CRED" -H 'Content-Type: application/json' \
  -d "{\"expires_at\":\"$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)\"}" | sed 's/.*"secret":"\([^"]*\)".*/\1/')
echo "$INVITE"          # starts with fmi2.acme.
```

`allow_quesma_etl` is off here so nothing in the trial is sealed to anyone but you. An invite is
single-use and its secret is shown once; if you lose `$INVITE`, run the last command again. On
Linux, `date -u -d '+1 hour'` replaces `date -u -v+1H`.

## 4. A developer machine

Plain Ubuntu, `curl`, a non-root user, and one sample Claude Code session from this repository's
test data so there is something to ship. The Dockerfile's `EOF` must start at the first column.

```sh
cat > Dockerfile <<'EOF'
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash dev
USER dev
WORKDIR /home/dev
COPY --chown=dev:dev session.jsonl /home/dev/.claude/projects/-home-dev-work-demo/9f8b7c6d-4e3a-4b2c-8d1e-0f9a8b7c6d5e.jsonl
EOF

curl -fsSL -o session.jsonl https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/src/e2e/testdata/golden/claude-2026-07/payloads/claude-code-transcripts-f91631a3882c.jsonl
docker build -t shipper-client .
```

## 5. Inside it: install, enroll, doctor, one run

Start the container with the two values a developer would receive from the operator, and get a
shell as `dev`:

```sh
docker run --rm -it -e FLEET_MANAGER_URL="$URL" -e SHIPPER_AUTH_KEY="$INVITE" shipper-client bash
```

At the container's prompt, the documented Linux install line. `--no-service` because a container
has no systemd; on a real machine the service runs the collection every 15 minutes. The token is
read from `SHIPPER_AUTH_KEY`, so this also enrolls:

```sh
curl -fsSLO https://raw.githubusercontent.com/QuesmaOrg/quesma-shipper/main/src/packaging/linux/install.sh
sh install.sh --no-service --server "$FLEET_MANAGER_URL"
export PATH=$HOME/.local/bin:$PATH
```

Expected: `installed 0.0.3-…`, the login output, and a note that the service was skipped. Then:

```sh
quesma-shipper doctor
```

Expected near the top: `Sending to acme at …`, `storage: one test file sent`, and `Claude Code 1
session in 1 project`. The test file is a real upload through a signed ticket, so this line alone
proves enrollment, ticket signing and the presigned PUT. Warnings about a missing background
service and missing enrichment databases are normal in a container. Then one collection run:

```sh
quesma-shipper run --once --drain
```

Expected: two lines ending in `shipped`, then `shipped 2 … failed 0` and `drain complete`.
`quesma-shipper log` shows the per-file decisions. `exit` removes the container.

## 6. Verify from the operator side

Back in `~/fm-trial`. Three objects: the session, the project map, and the heartbeat. Open the
session with a custodian identity; inside is the scrubbed file and its manifest.

```sh
B=$(sh deploy.sh aws output bucket --dir .)
aws s3 ls --recursive "s3://$B/v1/organization=acme/install="
KEY=$(aws s3api list-objects-v2 --bucket "$B" --prefix 'v1/organization=acme/install=' \
  --query "Contents[?contains(Key, 'claude-code-transcripts')].Key | [0]" --output text)
aws s3 cp "s3://$B/$KEY" object.age
age -d -i custodian-1.agekey object.age | zstd -d | tar -t        # manifest.json, payload
curl -sS "$URL/v1/admin/orgs/acme/installs" -H "Authorization: Bearer $CRED"   # "status":"active"
```

## 7. The admin UI

```sh
sh deploy.sh aws output admin_url --dir .          # https://…/admin/
sh deploy.sh aws output admin_credential --dir .   # fma1.…
```

Open the address, paste the credential on the **Connect to your fleet** page. The selector shows
**acme**; its **Configuration** tab is what step 3 created, **Invites** lists the invite as used,
and **Installs** shows the container as `active` with its hostname, platform and client version.
**Name this install** stores a name beside the install's objects in the bucket. The UI shows no
file content and cannot decrypt anything: that stays with the custodians, as in step 6.

## 8. Tear it down

The bucket is versioned, so every version has to go before Terraform will remove it. Then the
directory and the image.

```sh
B=$(sh deploy.sh aws output bucket --dir .)
for kind in Versions DeleteMarkers; do
  aws s3api list-object-versions --bucket "$B" \
    --query "{Objects: ${kind}[].{Key:Key,VersionId:VersionId}}" --output json > del.json
  grep -q VersionId del.json && aws s3api delete-objects --bucket "$B" --delete file://del.json >/dev/null
done
sh deploy.sh aws destroy --dir . -y

cd ~ && rm -rf ~/fm-trial && docker rmi shipper-client
```

Nothing remains in the account or on the laptop.

### Collection configuration compatibility

The admin config API accepts a typed `collection` object, for example
`{"mode":{"schedule":"15m"},"max_files_per_run":512,"drain_deadline":"30s"}`.
New writes are checked against the shared shipper-protocol authoring schema. The
transitional `authored_yaml` request field is still accepted and receives the same
validation; a request must not provide both fields. GET includes a generated
`authored_yaml` view for older dashboard clients during rollout.

Existing stored YAML is read without a write and retains the shipper's tolerant
read semantics (for example, an old `5s` schedule remains readable). A successful
write migrates it to `collection`. If the old document cannot be represented, GET
retains its original YAML and ETag, sets `collection_error`, and leaves the record
untouched. The admin form displays the original and offers **Rebuild collection
settings**; replacement happens only when Apply succeeds. Organization listing,
telemetry, and upload authorization remain available while it is repaired.

Deploy the shipper fallback release first, then fleet-manager, then the dashboard.
Once configs have been saved in typed form, rolling back to an older fleet-manager
binary requires restoring the corresponding versioned `config.json` objects first:
older strict record decoders do not recognize the new `collection` field.
