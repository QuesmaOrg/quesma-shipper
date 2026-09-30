#!/bin/sh
# Deploys Fleet Manager and its bucket into your AWS account or Google Cloud project with the
# Terraform template of this repository, without a clone: it downloads the template, applies it,
# and prints what the next step needs. Re-running upgrades. Usage: deploy.sh aws|gcp [options]
set -eu

REPO=QuesmaOrg/quesma-shipper
REF=${QUESMA_SHIPPER_REF:-main}
HOME_DIR=${QUESMA_FLEET_MANAGER_HOME:-$HOME/.quesma/fleet-manager}

main() {
	[ $# -ge 1 ] || { usage >&2; exit 2; }
	case $1 in
	aws | gcp) CLOUD=$1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "the first argument is the cloud: aws or gcp (see --help)" ;;
	esac

	ACTION=apply
	case ${1:-} in
	output | destroy | plan) ACTION=$1; shift ;;
	esac

	BUCKET= REGION= NAME= IMAGE= PROJECT= TELEMETRY= QUESMA_ETL= YES= OUTPUT_NAME=
	while [ $# -gt 0 ]; do
		case $1 in
		--bucket) BUCKET=$2; shift 2 ;;
		--region) REGION=$2; shift 2 ;;
		--name) NAME=$2; shift 2 ;;
		--image) IMAGE=$2; shift 2 ;;
		--project) PROJECT=$2; shift 2 ;;
		--telemetry) TELEMETRY=$2; shift 2 ;;
		--no-quesma-etl) QUESMA_ETL=false; shift ;;
		--quesma-etl) QUESMA_ETL=true; shift ;;
		--ref) REF=$2; shift 2 ;;
		--dir) HOME_DIR=$2; shift 2 ;;
		-y | --yes) YES=1; shift ;;
		-h | --help) usage; exit 0 ;;
		--*) die "unknown option $1 (see --help)" ;;
		*)
			[ "$ACTION" = output ] && [ -z "$OUTPUT_NAME" ] || die "unexpected argument $1 (see --help)"
			OUTPUT_NAME=$1; shift ;;
		esac
	done

	DIR=$HOME_DIR/$CLOUD
	VARS=$DIR/terraform.tfvars.json
	pick_terraform

	case $ACTION in
	output) run_output ;;
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
	cat <<EOF

Fleet Manager is up.

  service      $url
  admin UI     $(tf output -raw admin_url)
  bucket       $(tf output -raw bucket)
  credential   $(tf output -raw admin_credential)
               paste it into the admin UI; \`$0 $CLOUD output admin_credential\` prints it again

Next: open the admin UI, create your organization (two age recipients from two custodians), then
create an invite per machine. On each developer machine, install the shipper and enroll it:

  quesma-shipper login <invite> --server $url

Everything about this deployment lives in $DIR, including terraform.tfstate. Back that
directory up; run this command again to upgrade.
EOF
}

run_output() {
	[ -f "$DIR/terraform.tfstate" ] || die "nothing deployed in $DIR yet"
	if [ -n "$OUTPUT_NAME" ]; then tf output -raw "$OUTPUT_NAME"; else tf output; fi
}

run_destroy() {
	[ -f "$DIR/terraform.tfstate" ] || die "nothing deployed in $DIR yet"
	say "this removes the service, its roles and the bucket $(tf output -raw bucket)"
	say "a bucket holding objects is refused: empty it first if that is what you want"
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
	case $CLOUD in
	aws)
		[ -n "$BUCKET" ] || die "--bucket NAME is required: S3 bucket names are global, so pick one nobody has"
		[ -n "$REGION" ] || REGION=$(aws configure get region 2>/dev/null || true)
		[ -n "$REGION" ] || die "--region is required, and your AWS profile names none" ;;
	gcp)
		[ -n "$PROJECT" ] || die "--project ID is required"
		[ -n "$REGION" ] || REGION=europe-central2 ;;
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
	esac
	[ -z "$NAME" ] || v="$v  \"name\": \"$NAME\",
"
	[ -z "$IMAGE" ] || v="$v  \"$(image_var)\": \"$IMAGE\",
"
	[ -z "$TELEMETRY" ] || v="$v  \"default_telemetry_collector_url\": \"$TELEMETRY\",
"
	[ -z "$QUESMA_ETL" ] || v="$v  \"default_allow_quesma_etl\": $QUESMA_ETL,
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
	gcloud auth application-default print-access-token >/dev/null 2>&1 \
		|| die "no application default credentials: \`gcloud auth application-default login\`"
	account=$(gcloud config get-value account 2>/dev/null || true)
	say "Google Cloud project $PROJECT, region $REGION, as ${account:-?}"
}

# The template at REF, over whatever the last run left, so a re-run is an upgrade; the state file
# and the recorded variables are never touched.
fetch_template() {
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	say "fetching the $CLOUD template from $REPO at $REF"
	curl -fsSL "https://codeload.github.com/$REPO/tar.gz/$REF" | tar -xzf - -C "$tmp" \
		|| die "download of $REPO at $REF failed"
	src=$(find "$tmp" -type d -path "*/fleet-manager/terraform/$CLOUD" | head -1)
	[ -n "$src" ] || die "no fleet-manager/terraform/$CLOUD in $REPO at $REF"
	rm -f "$DIR"/*.tf
	cp "$src"/*.tf "$DIR"/
	[ -f "$DIR/.terraform.lock.hcl" ] || cp "$src/.terraform.lock.hcl" "$DIR/" 2>/dev/null || true
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

  deploy.sh aws --bucket NAME [--region REGION]      AWS: S3 bucket, roles, ECS Express service
  deploy.sh gcp --project ID [--region REGION]       Google Cloud: GCS bucket, roles, Cloud Run
  deploy.sh aws|gcp output [NAME]                    the outputs, or one of them (admin_credential)
  deploy.sh aws|gcp plan                             show what a run would change
  deploy.sh aws|gcp destroy                          remove it all; refuses a bucket with objects

Options, remembered between runs:
  --name NAME             what the service and its roles are called (default fleet-manager)
  --image IMAGE           the container image, by digest if you build your own
  --bucket NAME           on gcp, instead of <project>-trajectories
  --no-quesma-etl         do not seal uploads to Quesma's recipient unless an organization asks
  --telemetry URL         forward shipper telemetry there for organizations that name nowhere
  --ref REF               the template's git ref (default main); --dir DIR where state is kept
  -y, --yes               apply without asking

Needs: terraform or tofu, curl, and the aws or gcloud CLI signed in to the target account.
EOF
}

die() { printf 'deploy: %s\n' "$*" >&2; exit 1; }
say() { printf 'deploy: %s\n' "$*"; }

main "$@"
