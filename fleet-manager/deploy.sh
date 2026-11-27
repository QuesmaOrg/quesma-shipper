#!/bin/sh
# Deploys Fleet Manager and its bucket into your AWS account, Google Cloud project (Terraform) or
# Cloudflare account (wrangler), without a clone: it downloads the template, applies it, and
# prints what the next step needs. Re-running upgrades. Usage: deploy.sh aws|gcp|cloudflare [options]
set -eu

REPO=QuesmaOrg/quesma-shipper
REF=${QUESMA_SHIPPER_REF:-}
HOME_DIR=${QUESMA_FLEET_MANAGER_HOME:-$HOME/.quesma/fleet-manager}

main() {
	[ $# -ge 1 ] || { usage >&2; exit 2; }
	case $1 in
	aws | gcp | cloudflare) CLOUD=$1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "the first argument is the cloud: aws, gcp or cloudflare (see --help)" ;;
	esac

	ACTION=apply
	case ${1:-} in
	output | destroy | plan | analytics-key) ACTION=$1; shift ;;
	esac

	BUCKET= REGION= NAME= IMAGE= PROJECT= TELEMETRY= QUESMA_ETL= ANALYTICS_KEY= YES= ARG= ACCOUNT= JURISDICTION=
	while [ $# -gt 0 ]; do
		case $1 in
		--bucket) BUCKET=$2; shift 2 ;;
		--region) REGION=$2; shift 2 ;;
		--account) ACCOUNT=$2; shift 2 ;;
		--jurisdiction) JURISDICTION=$2; shift 2 ;;
		--name) NAME=$2; shift 2 ;;
		--image) IMAGE=$2; shift 2 ;;
		--project) PROJECT=$2; shift 2 ;;
		--telemetry) TELEMETRY=$2; shift 2 ;;
		--no-quesma-etl) QUESMA_ETL=false; shift ;;
		--quesma-etl) QUESMA_ETL=true; shift ;;
		--analytics-key) ANALYTICS_KEY=true; shift ;;
		--ref) REF=$2; shift 2 ;;
		--dir) HOME_DIR=$2; shift 2 ;;
		-y | --yes) YES=1; shift ;;
		-h | --help) usage; exit 0 ;;
		--*) die "unknown option $1 (see --help)" ;;
		*)
			case $ACTION in output | analytics-key) ;; *) die "unexpected argument $1 (see --help)" ;; esac
			[ -z "$ARG" ] || die "unexpected argument $1 (see --help)"
			ARG=$1; shift ;;
		esac
	done

	DIR=$HOME_DIR/$CLOUD
	VARS=$DIR/terraform.tfvars.json
	[ -n "$REF" ] || REF=$(cat "$DIR/ref" 2>/dev/null || echo main)
	if [ "$CLOUD" = cloudflare ]; then
		VARS=$DIR/settings.json
		case $ACTION in
		apply) cf_apply ;;
		output) cf_output ;;
		destroy) cf_destroy ;;
		*) die "$ACTION is for aws and gcp" ;;
		esac
		return
	fi
	pick_terraform

	case $ACTION in
	output) run_output ;;
	analytics-key) run_analytics_key ;;
	destroy) run_destroy ;;
	plan | apply) run_apply ;;
	esac
}

run_apply() {
	need curl tar
	mkdir -p "$DIR"
	collect_vars
	preflight_"$CLOUD"
	fetch_template
	write_vars

	say "deploying with $TF in $DIR"
	tf init -upgrade -input=false >"$DIR/init.log" 2>&1 || { cat "$DIR/init.log"; die "$TF init failed"; }
	if [ "$ACTION" = plan ]; then
		tf plan -input=false
		return
	fi
	tf apply -input=false ${YES:+-auto-approve}

	url=$(tf output -raw service_url)
	wait_healthy "$url"
	print_up "$url" "$(tf output -raw admin_url)" "$(tf output -raw bucket)" "$(tf output -raw admin_credential)" \
		"Everything about this deployment lives in $DIR, including terraform.tfstate. Back that
directory up; run this command again to upgrade."
}

# URL, admin URL, bucket, credential, then the closing paragraph about this deployment's state.
print_up() {
	cat <<EOF

Fleet Manager is up.

  service      $1
  admin UI     $2
  bucket       $3
  credential   $4
               paste it into the admin UI; \`$0 $CLOUD output admin_credential\` prints it again

Next: open the admin UI, create your organization (two age recipients from two custodians), then
create an invite per machine. On each developer machine, install the shipper and enroll it:

  quesma-shipper login <invite> --server $1

$5
EOF
}

run_output() {
	[ -f "$DIR/terraform.tfstate" ] || die "nothing deployed in $DIR yet"
	if [ -n "$ARG" ]; then tf output -raw "$ARG"; else tf output; fi
}

# An organization's analytics identity, printed only as KMS ciphertext beside its public recipient.
# The private key lives in this shell's memory and pipes; printf is a builtin, so it is in no argv.
run_analytics_key() {
	[ "$CLOUD" = aws ] || die "analytics-key needs AWS KMS; Google Cloud is not supported yet"
	printf '%s\n' "$ARG" | grep -Eqx '[a-z0-9][a-z0-9._-]{0,63}' \
		|| die "analytics-key takes the organization slug: $0 aws analytics-key acme"
	[ -f "$DIR/terraform.tfstate" ] || die "nothing deployed in $DIR yet"
	need aws age-keygen
	# The CLI's history records every call's parameters, the identity on stdin included.
	[ "$(aws configure get cli_history 2>/dev/null || true)" != enabled ] \
		|| die "AWS CLI history is on and would record the identity: set cli_history = disabled first"
	key=$(tf output -raw analytics_key_arn 2>/dev/null || true)
	[ -n "$key" ] || die "this deployment has no analytics key: run \`$0 aws --analytics-key\` first"
	identity=$(age-keygen 2>/dev/null | grep '^AGE-SECRET-KEY-') || die "age-keygen failed"
	recipient=$(printf '%s\n' "$identity" | age-keygen -y) || die "age-keygen -y failed"
	ciphertext=$(printf '%s\n' "$identity" | aws kms encrypt --region "$(echo "$key" | cut -d: -f4)" \
		--key-id "$key" --plaintext fileb:///dev/stdin --query CiphertextBlob --output text \
		--encryption-context "purpose=analytics-identity,organization=$ARG") || die "aws kms encrypt failed"
	identity=
	printf '{"organization":"%s","recipient":"%s","ciphertext":"%s","kms_key":"%s"}\n' "$ARG" "$recipient" "$ciphertext" "$key"
}

run_destroy() {
	[ -f "$DIR/terraform.tfstate" ] || die "nothing deployed in $DIR yet"
	say "this removes the service, its roles and the bucket $(tf output -raw bucket)"
	say "a bucket holding objects is refused: empty it first if that is what you want"
	key=$(tf output -raw analytics_key_arn 2>/dev/null || true)
	if [ -n "$key" ]; then
		# Terraform schedules the key's deletion even when it refuses the bucket, and a new key opens
		# nothing sealed under the old one. A versioned bucket is empty only with no versions left.
		# A bucket an earlier, partial destroy already removed holds nothing.
		errors=$(mktemp)
		if ! left=$(aws s3api list-object-versions --region "$(echo "$key" | cut -d: -f4)" --bucket "$(tf output -raw bucket)" \
			--max-items 1 --query '[length(Versions || `[]`), length(DeleteMarkers || `[]`)]' --output text 2>"$errors"); then
			left=$(cat "$errors")
			rm -f "$errors"
			case $left in
			*NoSuchBucket*) left="0 0" ;;
			*) die "could not check whether the bucket still holds objects: $left" ;;
			esac
		fi
		rm -f "$errors"
		[ "$(echo $left)" = "0 0" ] \
			|| die "the bucket still holds objects, and destroying would delete the analytics key every organization's identity is held under: empty the bucket first if that is what you want"
	fi
	tf destroy -input=false ${YES:+-auto-approve}
}

# Flags win, then what the last run recorded, then the cloud's own defaults.
collect_vars() {
	[ -n "$BUCKET" ] || BUCKET=$(stored bucket)
	[ -n "$REGION" ] || REGION=$(stored region)
	[ -n "$NAME" ] || NAME=$(stored name)
	[ -n "$IMAGE" ] || IMAGE=$(stored "$(image_var)")
	[ -n "$PROJECT" ] || PROJECT=$(stored project)
	[ -n "$TELEMETRY" ] || TELEMETRY=$(stored default_telemetry_collector_url)
	[ -n "$QUESMA_ETL" ] || QUESMA_ETL=$(stored default_allow_quesma_etl)
	# A new deployment states Quesma's recipient off. One made before leaves it to the template,
	# whose default stays on, so an upgrade with this script or an older copy changes nothing.
	[ -n "$QUESMA_ETL" ] || [ -f "$DIR/terraform.tfstate" ] || [ -f "$DIR/url" ] || QUESMA_ETL=false
	[ -n "$ANALYTICS_KEY" ] || ANALYTICS_KEY=$(stored analytics_key)
	case $CLOUD in
	aws)
		[ -n "$BUCKET" ] || die "--bucket NAME is required: S3 bucket names are global, so pick one nobody has"
		[ -n "$REGION" ] || REGION=$(aws configure get region 2>/dev/null || true)
		[ -n "$REGION" ] || die "--region is required, and your AWS profile names none" ;;
	gcp)
		[ -n "$PROJECT" ] || die "--project ID is required"
		[ -n "$REGION" ] || REGION=europe-central2 ;;
	cloudflare) cf_collect ;;
	esac
}

stored() {
	[ -f "$VARS" ] || return 0
	sed -n "s/^ *\"$1\": *\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}\$/\1/p" "$VARS" | head -1
}

write_vars() {
	v=
	case $CLOUD in
	aws) v="  \"bucket\": \"$BUCKET\",
  \"region\": \"$REGION\",
" ;;
	gcp)
		v="  \"project\": \"$PROJECT\",
  \"region\": \"$REGION\",
"
		[ -z "$BUCKET" ] || v="$v  \"bucket\": \"$BUCKET\",
" ;;
	cloudflare)
		v="  \"bucket\": \"$BUCKET\",
  \"account\": \"$ACCOUNT\",
"
		[ -z "$JURISDICTION" ] || v="$v  \"jurisdiction\": \"$JURISDICTION\",
" ;;
	esac
	[ -z "$NAME" ] || v="$v  \"name\": \"$NAME\",
"
	[ -z "$IMAGE" ] || v="$v  \"$(image_var)\": \"$IMAGE\",
"
	[ -z "$TELEMETRY" ] || v="$v  \"default_telemetry_collector_url\": \"$TELEMETRY\",
"
	[ -z "$QUESMA_ETL" ] || v="$v  \"default_allow_quesma_etl\": $QUESMA_ETL,
"
	[ -z "$ANALYTICS_KEY" ] || v="$v  \"analytics_key\": $ANALYTICS_KEY,
"
	{ echo '{'; printf '%s' "$v" | sed '$ s/,$//'; echo '}'; } >"$VARS"
}

image_var() { [ "$CLOUD" = aws ] && echo image_uri || echo image; }

preflight_aws() {
	need aws
	identity=$(aws sts get-caller-identity --output text --query "join(' ', [Account, Arn])" 2>/dev/null) \
		|| die "no AWS session: set AWS_PROFILE, then \`aws sso login\` or \`aws configure\`"
	say "AWS account ${identity%% *}, region $REGION, as ${identity#* }"
	vpc=$(aws ec2 describe-vpcs --region "$REGION" --filters Name=is-default,Values=true \
		--query 'Vpcs[0].VpcId' --output text 2>/dev/null || true)
	case $vpc in
	'' | None) die "no default VPC in $REGION; ECS Express Mode needs one (aws ec2 create-default-vpc)" ;;
	esac
}

preflight_gcp() {
	need gcloud
	token=$(gcloud auth application-default print-access-token 2>/dev/null) \
		|| die "no application default credentials: \`gcloud auth application-default login\`"
	account=$(curl -fsS -H "Authorization: Bearer $token" https://www.googleapis.com/oauth2/v3/tokeninfo \
		2>/dev/null | sed -n 's/.*"email": *"\([^"]*\)".*/\1/p')
	# Google answers 403 both for a project that does not exist and for one this identity cannot see.
	curl -fsS -H "Authorization: Bearer $token" \
		"https://cloudresourcemanager.googleapis.com/v1/projects/$PROJECT" >/dev/null 2>&1 \
		|| die "project $PROJECT not found or not visible to ${account:-these credentials}; --project takes" \
			"an existing project ID (\`gcloud projects list\`), and this script never creates a project"
	billing=$(curl -fsS -H "Authorization: Bearer $token" \
		"https://cloudbilling.googleapis.com/v1/projects/$PROJECT/billingInfo" 2>/dev/null | tr -d ' \n')
	case $billing in
	*'"billingEnabled":true'*) ;;
	*'"name":'*) die "project $PROJECT has no billing account linked, and Cloud Run and Cloud Storage" \
		"need one: \`gcloud billing projects link $PROJECT --billing-account ID\`, with the ID from" \
		"\`gcloud billing accounts list\`" ;;
	*) say "could not check billing on $PROJECT; Terraform fails later if none is linked" ;;
	esac
	say "Google Cloud project $PROJECT, region $REGION, as ${account:-?}"
}

# The template at REF, over whatever the last run left, so a re-run is an upgrade; the state file
# and the recorded variables are never touched.
fetch_template() {
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	sub=terraform/$CLOUD
	[ "$CLOUD" != cloudflare ] || sub=cloudflare
	say "fetching the $CLOUD template from $REPO at $REF"
	curl -fsSL "https://codeload.github.com/$REPO/tar.gz/$REF" | tar -xzf - -C "$tmp" \
		|| die "download of $REPO at $REF failed"
	src=$(find "$tmp" -type d -path "*/fleet-manager/$sub" | head -1)
	[ -n "$src" ] || die "no fleet-manager/$sub in $REPO at $REF"
	if [ "$CLOUD" = cloudflare ]; then
		rm -rf "$DIR/worker"
		cp -R "$src" "$DIR/worker"
	else
		rm -f "$DIR"/*.tf
		cp "$src"/*.tf "$DIR"/
		[ -f "$DIR/.terraform.lock.hcl" ] || cp "$src/.terraform.lock.hcl" "$DIR/" 2>/dev/null || true
	fi
	rm -rf "$tmp"
	echo "$REF" >"$DIR/ref"
}

wait_healthy() {
	path=/healthz
	[ "$CLOUD" = aws ] || path=/health
	say "waiting for ${1%/}$path"
	i=0
	while ! curl -fsS --max-time 5 "${1%/}$path" >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "the service did not answer within 5 minutes; check its logs"
		sleep 5
	done
}

# Cloudflare: wrangler instead of Terraform. The state is the remembered settings, the admin
# credential and the service URL; Cloudflare keeps the R2 keys as Worker secrets.
WRANGLER_VERSION=4.157.0
DEFAULT_IMAGE=docker.io/quesma/fleet-manager:latest

cf_apply() {
	need curl tar openssl docker
	docker buildx version >/dev/null 2>&1 || die "needs docker with buildx, to resolve the image digest"
	[ -n "${WRANGLER:-}" ] || need npx
	mkdir -p "$DIR"
	chmod 700 "$DIR"
	collect_vars
	jurisdiction=${JURISDICTION:+--jurisdiction $JURISDICTION}
	fetch_template
	image=$(cf_pin_image "$IMAGE")
	printf '# Written by deploy.sh: the image this deployment runs.\nFROM %s\n' "$image" >"$DIR/worker/Dockerfile"
	say "Cloudflare account $ACCOUNT, Worker $NAME, bucket $BUCKET, image $image"
	[ -f "$DIR/admin_credential" ] \
		|| say "no admin credential in $DIR: a new one replaces whatever this bucket accepted before"
	confirm deploy
	write_vars

	if ! wr r2 bucket info "$BUCKET" $jurisdiction >"$DIR/wrangler.log" 2>&1; then
		say "creating the R2 bucket $BUCKET"
		wr_logged "could not create the R2 bucket $BUCKET" r2 bucket create "$BUCKET" $jurisdiction
	fi
	if [ ! -f "$DIR/url" ] && { [ -z "${FLEET_MANAGER_R2_ACCESS_KEY_ID:-}" ] || [ -z "${FLEET_MANAGER_R2_SECRET_ACCESS_KEY:-}" ]; }; then
		die "FLEET_MANAGER_R2_ACCESS_KEY_ID and FLEET_MANAGER_R2_SECRET_ACCESS_KEY are required on the first" \
			"deploy: the bucket $BUCKET exists now, so in the dashboard, R2 > Manage API tokens, create an" \
			"Object Read & Write token for it, export its two keys and run this again"
	fi
	if [ ! -f "$DIR/admin_credential" ]; then
		(umask 077 && printf 'fma1.%s' "$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')" >"$DIR/admin_credential")
		say "minted a new admin credential; any earlier one for this bucket stops working"
	fi
	digest=$(openssl dgst -sha256 -r <"$DIR/admin_credential" | cut -d' ' -f1)
	printf '{"schema":1,"secret_digest":"%s"}' "$digest" | wr_logged "could not write the admin credential digest to $BUCKET" \
		r2 object put "$BUCKET/v1/control/admin/credential.json" --pipe --remote --content-type application/json $jurisdiction

	secrets=$DIR/secrets.json
	# dash skips the EXIT trap on a signal, so each signal becomes an exit that runs it.
	trap 'rm -f "$secrets"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP
	(umask 077 && cf_secrets >"$secrets")
	say "deploying with wrangler $WRANGLER_VERSION; the first deploy builds the image and can take minutes"
	wr deploy --config "$DIR/worker/wrangler.jsonc" --name "$NAME" --secrets-file "$secrets" >"$DIR/deploy.log" 2>&1 \
		|| { cat "$DIR/deploy.log"; die "wrangler deploy failed"; }
	rm -f "$secrets"
	url=$(grep -Eo 'https://[A-Za-z0-9.-]+\.workers\.dev' "$DIR/deploy.log" | head -1)
	[ -n "$url" ] || { cat "$DIR/deploy.log"; die "wrangler deploy printed no workers.dev URL"; }
	echo "$url" >"$DIR/url"
	wait_healthy "$url"
	print_up "$url" "$url/admin/" "$BUCKET" "$(cat "$DIR/admin_credential")" \
		"R2 has no object versioning: an object a shipper uploads again replaces the earlier one.
Everything about this deployment lives in $DIR, including the only copy of the admin
credential. Back that directory up; run this command again to upgrade."
}

cf_output() {
	[ -f "$DIR/url" ] || die "nothing deployed in $DIR yet"
	url=$(cat "$DIR/url")
	case $ARG in
	'') printf 'admin_credential = %s\nadmin_url = %s/admin/\nbucket = %s\nservice_url = %s\n' \
		"$(cat "$DIR/admin_credential")" "$url" "$(stored bucket)" "$url" ;;
	admin_credential) cat "$DIR/admin_credential"; echo ;;
	admin_url) echo "$url/admin/" ;;
	bucket) stored bucket ;;
	service_url) echo "$url" ;;
	*) die "no output $ARG: admin_credential, admin_url, bucket or service_url" ;;
	esac
}

cf_destroy() {
	[ -f "$DIR/url" ] || die "nothing deployed in $DIR yet"
	[ -n "${WRANGLER:-}" ] || need npx
	collect_vars
	say "this removes the Worker $NAME and its container; the bucket $BUCKET and its objects are kept"
	confirm continue
	# Run non-interactively, wrangler deletes the Worker but only lists the Container applications it
	# owned, which keep running and billing; they are deleted here by the ids it lists.
	wr_logged "wrangler delete failed" delete --name "$NAME" --force </dev/null
	if grep -q 'not check whether' "$DIR/wrangler.log"; then
		cat "$DIR/wrangler.log"
		die "the Worker is gone, but its container may still run: delete it in the dashboard under Containers"
	fi
	ids=$(sed -n 's/^[[:space:]]*- .* (\([0-9A-Za-z-]*\))[[:space:]]*$/\1/p' "$DIR/wrangler.log")
	if [ -z "$ids" ] && grep -q 'running and billing' "$DIR/wrangler.log"; then
		cat "$DIR/wrangler.log"
		die "the Worker is gone, but no container id could be read: delete it in the dashboard under Containers"
	fi
	for id in $ids; do
		(CI=true && export CI && wr_logged "could not delete the container application $id" containers delete "$id" </dev/null)
		say "deleted the container application $id"
	done
	rm -f "$DIR/url"
	say "removed; delete the bucket in the dashboard once you no longer need what it holds"
}

# What only Cloudflare has, after collect_vars' shared precedence; there is no template to default.
cf_collect() {
	[ -z "$ANALYTICS_KEY" ] || die "--analytics-key needs AWS KMS"
	[ -n "$ACCOUNT" ] || ACCOUNT=$(stored account)
	[ -n "$ACCOUNT" ] || ACCOUNT=${CLOUDFLARE_ACCOUNT_ID:-}
	[ -n "$JURISDICTION" ] || JURISDICTION=$(stored jurisdiction)
	[ -n "$NAME" ] || NAME=fleet-manager
	[ -n "$IMAGE" ] || IMAGE=$DEFAULT_IMAGE
	printf '%s\n' "$BUCKET" | grep -Eqx '[a-z0-9][a-z0-9-]{1,61}[a-z0-9]' \
		|| die "--bucket NAME is required: 3 to 63 lowercase letters, digits and hyphens"
	printf '%s\n' "$ACCOUNT" | grep -Eqx '[0-9a-f]{32}' \
		|| die "--account ID (or CLOUDFLARE_ACCOUNT_ID) is required: the 32-character ID in the dashboard URL"
	case $JURISDICTION in '' | eu | fedramp) ;; *) die "--jurisdiction is eu or fedramp, not $JURISDICTION" ;; esac
	case $TELEMETRY in *[\"\\]*) die "--telemetry URL may not contain quotes or backslashes" ;; esac
	# The R2 token is scoped to one bucket and the secrets belong to one Worker.
	if [ -f "$DIR/url" ]; then
		for pair in "bucket:$BUCKET" "name:$NAME" "jurisdiction:$JURISDICTION"; do
			key=${pair%%:*} new=${pair#*:} old=$(stored "${pair%%:*}")
			[ "$old" = "$new" ] || die "--$key cannot change on a deployed Fleet Manager (${old:-none} to" \
				"${new:-none}): run \`$0 cloudflare destroy\` first, or use another --dir"
		done
	fi
	export CLOUDFLARE_ACCOUNT_ID=$ACCOUNT
}

# The container's environment, as the Worker secrets src/index.js forwards. Wrangler keeps secrets
# a run omits, so a re-run without the R2 keys in the environment keeps the stored ones.
cf_secrets() {
	for name in FLEET_MANAGER_R2_ACCESS_KEY_ID FLEET_MANAGER_R2_SECRET_ACCESS_KEY; do
		eval "value=\${$name:-}"
		case $value in *[\"\\]*) die "$name may not contain quotes or backslashes" ;; esac
	done
	printf '{"FLEET_MANAGER_BUCKET":"%s","FLEET_MANAGER_ACCOUNT":"%s"' "$BUCKET" "$ACCOUNT"
	[ -z "$JURISDICTION" ] || printf ',"FLEET_MANAGER_R2_JURISDICTION":"%s"' "$JURISDICTION"
	[ -z "$QUESMA_ETL" ] || printf ',"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL":"%s"' "$QUESMA_ETL"
	[ -z "$TELEMETRY" ] || printf ',"FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL":"%s"' "$TELEMETRY"
	[ -z "${FLEET_MANAGER_R2_ACCESS_KEY_ID:-}" ] || printf ',"FLEET_MANAGER_R2_ACCESS_KEY_ID":"%s"' "$FLEET_MANAGER_R2_ACCESS_KEY_ID"
	[ -z "${FLEET_MANAGER_R2_SECRET_ACCESS_KEY:-}" ] || printf ',"FLEET_MANAGER_R2_SECRET_ACCESS_KEY":"%s"' "$FLEET_MANAGER_R2_SECRET_ACCESS_KEY"
	printf '}\n'
}

# A tag is resolved to the digest it names now, so the deployment runs exactly what was resolved.
cf_pin_image() {
	case $1 in *@sha256:*) echo "$1"; return ;; esac
	base=$1
	case ${1##*/} in *:*) base=${1%:*} ;; esac
	# Only "not found" from the registry means absent; any other failure stops rather than guessing.
	if out=$(docker buildx imagetools inspect "$1" --format '{{json .Manifest.Digest}}' 2>&1); then
		echo "$base@$(printf '%s' "$out" | tr -d '"')"
		return
	fi
	case $out in
	*": not found"* | *"repository does not exist"*) ;;
	*) die "cannot ask the registry about $1, so not guessing: $out" ;;
	esac
	docker image inspect "$1" >/dev/null 2>&1 || die "image $1 is neither in a registry nor in local Docker"
	say "image $1 exists only in local Docker, so this deployment is not pinned to a digest" >&2
	echo "$1"
}

wr() {
	if [ -n "${WRANGLER:-}" ]; then "$WRANGLER" "$@"; else npx --yes "wrangler@$WRANGLER_VERSION" "$@"; fi
}

# wrangler with its output kept in wrangler.log and shown only when it fails with message $1.
wr_logged() {
	msg=$1
	shift
	wr "$@" >"$DIR/wrangler.log" 2>&1 || { cat "$DIR/wrangler.log"; die "$msg"; }
}

confirm() {
	[ -z "$YES" ] || return 0
	printf 'deploy: type yes to %s: ' "$1"
	read -r answer || answer=
	[ "$answer" = yes ] || die "not confirmed"
}

pick_terraform() {
	if [ -n "${TF:-}" ]; then need "$TF"
	elif command -v terraform >/dev/null 2>&1; then TF=terraform
	elif command -v tofu >/dev/null 2>&1; then TF=tofu
	else die "needs terraform or tofu on PATH"
	fi
}

tf() { "$TF" -chdir="$DIR" "$@"; }

need() {
	for t in "$@"; do command -v "$t" >/dev/null 2>&1 || die "needs $t on PATH"; done
}

usage() {
	cat <<EOF
Deploys Fleet Manager into your cloud account and prints the admin URL and credential.

  deploy.sh aws --bucket NAME [--region REGION]                  AWS: S3 bucket, roles, ECS Express service
  deploy.sh gcp --project ID [--bucket NAME] [--region REGION]   Google Cloud: GCS bucket, roles, Cloud Run
  deploy.sh cloudflare --bucket NAME [--account ID]              Cloudflare: R2 bucket, Worker, Container
  deploy.sh aws|gcp|cloudflare output [NAME]                     the outputs, or one of them (admin_credential)
  deploy.sh aws analytics-key ORG                                an organization's analytics identity, as KMS ciphertext
  deploy.sh aws|gcp plan                                         show what a run would change
  deploy.sh aws|gcp destroy                                      remove it all; refuses a bucket with objects
  deploy.sh cloudflare destroy                                   remove the Worker and container; keeps the bucket

Options, remembered between runs:
  --name NAME             what the service and its roles are called (default fleet-manager)
  --image IMAGE           the container image, by digest if you build your own
  --bucket NAME           on gcp, instead of <project>-trajectories
  --quesma-etl            seal uploads to Quesma's recipient for organizations that do not choose
  --no-quesma-etl         do not (the default for a new deployment)
  --analytics-key         on aws, create the KMS key analytics identities are encrypted under
  --account ID            on cloudflare, the account ID (default \$CLOUDFLARE_ACCOUNT_ID)
  --jurisdiction J        on cloudflare, an R2 jurisdiction: eu or fedramp
  --telemetry URL         forward shipper telemetry there for organizations that name nowhere
  --ref REF               the template's git ref, main unless set once; --dir DIR where state is kept
  -y, --yes               apply (or on cloudflare, deploy or destroy) without asking

Needs: terraform or tofu, curl, and the aws or gcloud CLI signed in to the target account.
On cloudflare instead: Docker, npx (Node.js), curl, openssl, a Workers Paid plan, CLOUDFLARE_API_TOKEN
or \`npx wrangler login\`, and on the first run FLEET_MANAGER_R2_ACCESS_KEY_ID and
FLEET_MANAGER_R2_SECRET_ACCESS_KEY from an R2 Object Read & Write token for the bucket.
EOF
}

die() { printf 'deploy: %s\n' "$*" >&2; exit 1; }
say() { printf 'deploy: %s\n' "$*"; }

main "$@"
