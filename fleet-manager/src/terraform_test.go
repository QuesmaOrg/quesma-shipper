package main

import (
	"os"
	"strings"
	"testing"
)

func TestTerraformPinsVersioningAndWriteOnlyDataPermissions(t *testing.T) {
	cases := []struct {
		file                string
		required, forbidden []string
	}{
		{"../terraform/aws/main.tf", []string{"status = \"Enabled\"", "TrajectoryWriteAndTagOnly", "s3:PutObject", "s3:PutObjectTagging", "s3:GetBucketVersioning", "aws_ecs_express_gateway_service", "AmazonECSInfrastructureRoleforExpressGatewayServices", "health_check_path       = \"/healthz\""},
			[]string{"s3:GetObjectVersion", "s3:DeleteObject", "aws_lambda_", "aws_vpc", "aws_lb", "aws_security_group", "aws_ecs_service"}},
		{"../terraform/gcp/main.tf", []string{"google_project_service", "versioning {", "enabled = true", "storage.objects.create", "storage.objects.delete", "storage.buckets.get", "serviceAccountTokenCreator", "allUsers", "path = \"/health\""},
			[]string{"storage.objects.getIamPolicy", "storage.objects.setIamPolicy"}},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, required := range tc.required {
			if !strings.Contains(text, required) {
				t.Errorf("%s does not contain %q", tc.file, required)
			}
		}
		for _, forbidden := range tc.forbidden {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s unexpectedly contains %q", tc.file, forbidden)
			}
		}
	}
}

// The one grant in the AWS template that lets anything outside the account read trajectory
// payloads: the bucket policy a deployment may attach for a Quesma-run ETL. It is optional, and
// what it may say is narrow, so it is pinned here rather than left to review.
//
// The check above forbids s3:GetObjectVersion anywhere in the file, which keeps this to current
// versions -- the reader detects a re-shipped object by ETag and never asks for a version id.
func TestTerraformExternalReaderGrantIsReadOnlyAndScoped(t *testing.T) {
	text, vars := "", ""
	for _, f := range []struct {
		path string
		into *string
	}{{"../terraform/aws/main.tf", &text}, {"../terraform/aws/variables.tf", &vars}} {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		*f.into = string(raw)
	}

	// Granting nobody is the default: a deployment holds a customer's trajectories.
	for _, required := range []string{"variable \"etl_reader_role_arns\"", "default     = []"} {
		if !strings.Contains(vars, required) {
			t.Errorf("../terraform/aws/variables.tf does not contain %q", required)
		}
	}

	// It is optional, and it is a bucket policy rather than a role to assume.
	for _, required := range []string{
		"resource \"aws_s3_bucket_policy\" \"etl_readers\"",
		"count = length(var.etl_reader_role_arns) > 0 ? 1 : 0",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("../terraform/aws/main.tf does not contain %q", required)
		}
	}

	// Every statement in it reads, and it names objects only through etl_readable_objects, so
	// that list is the whole of what a reader may open.
	grant := text[strings.Index(text, "data \"aws_iam_policy_document\" \"etl_readers\""):]
	grant = grant[:strings.Index(grant, "resource \"aws_s3_bucket_policy\" \"etl_readers\"")]
	for _, forbidden := range []string{
		"s3:Put", "s3:Delete", "s3:Abort", "s3:Restore", "s3:Replicate", "s3:*",
		"s3:GetBucketPolicy", "s3:GetEncryptionConfiguration",
		"fleet.arn}/",
	} {
		if strings.Contains(grant, forbidden) {
			t.Errorf("the external reader grant unexpectedly contains %q", forbidden)
		}
	}

	// Exactly two kinds of object: what is under an install's root, and each organization's
	// config.json. The rest of the control prefix -- invites, grants, install records, credential
	// digests -- is not a reader's to open, and "v1/*" would have been all of it.
	start := strings.Index(text, "etl_readable_objects = [")
	if start < 0 {
		t.Fatal("../terraform/aws/main.tf does not define etl_readable_objects")
	}
	readable := text[start : start+strings.Index(text[start:], "]")+1]
	want := "etl_readable_objects = [\n" +
		"    \"${aws_s3_bucket.fleet.arn}/${local.data_prefix}*\",\n" +
		"    \"${aws_s3_bucket.fleet.arn}/v1/organization=*/control/config.json\",\n" +
		"  ]"
	if readable != want {
		t.Errorf("etl_readable_objects is\n%s\nwant\n%s", readable, want)
	}
	if !strings.Contains(text, "data_prefix            = \"v1/organization=*/install=\"") {
		t.Error("data_prefix no longer ends at an install's root")
	}

	// And a Deny for everything else. An Allow in a bucket policy cannot narrow a reader in the
	// same account whose own role grants the archive prefix; only a Deny does.
	for _, required := range []string{
		"resources = local.etl_readable_objects",
		"effect        = \"Deny\"",
		"not_resources = local.etl_readable_objects",
	} {
		if !strings.Contains(grant, required) {
			t.Errorf("the external reader grant does not contain %q", required)
		}
	}
}

// The service compares a presented credential with the stored digest and never writes one; this
// Terraform provisions both. A runtime identity that could write them could mint its own
// administrator, so each template grants them read and nothing else.
func TestTerraformCredentialRecordsAreReadOnlyToTheRuntime(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	statement := func(text, sid string) string {
		t.Helper()
		start := strings.Index(text, "Sid    = \""+sid+"\"")
		if start < 0 {
			t.Fatalf("no %s statement", sid)
		}
		end := strings.Index(text[start:], "\n      },")
		return text[start : start+end]
	}

	aws := read("../terraform/aws/main.tf")
	for _, key := range []string{"local.admin_credential_key", "local.reporter_credential_key"} {
		if strings.Contains(statement(aws, "ControlObjects"), key) {
			t.Errorf("aws: ControlObjects, which writes, covers %s", key)
		}
		if !strings.Contains(statement(aws, "CredentialsRead"), key) {
			t.Errorf("aws: CredentialsRead does not cover %s", key)
		}
	}
	if got := statement(aws, "CredentialsRead"); !strings.Contains(got, "Action = \"s3:GetObject\"") {
		t.Errorf("aws: CredentialsRead grants more than s3:GetObject:\n%s", got)
	}

	gcp := read("../terraform/gcp/main.tf")
	control := gcp[strings.Index(gcp, "resource \"google_storage_bucket_iam_member\" \"control\""):]
	control = control[:strings.Index(control, "\n}\n")]
	for _, key := range []string{"local.admin_credential_key", "local.reporter_credential_key"} {
		if strings.Contains(control, key) {
			t.Errorf("gcp: the control binding, whose role writes, covers %s", key)
		}
	}
	for _, required := range []string{
		"resource \"google_project_iam_custom_role\" \"credentials\" {\n  role_id     = \"${replace(var.name, \"-\", \"_\")}_credentials\"\n  title       = \"Fleet manager credential records\"\n  permissions = [\"storage.objects.get\"]\n}",
		"role   = google_project_iam_custom_role.credentials.name",
		"expression = \"resource.name == '${local.object_root}${local.admin_credential_key}' || resource.name == '${local.object_root}${local.reporter_credential_key}'\"",
	} {
		if !strings.Contains(gcp, required) {
			t.Errorf("gcp: does not contain %q", required)
		}
	}
}

func TestTerraformPublishesSharedOperatorTarget(t *testing.T) {
	for _, file := range []string{"../terraform/aws/outputs.tf", "../terraform/gcp/outputs.tf"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, output := range []string{"provider", "bucket", "region", "service_url", "admin_url", "admin_credential"} {
			if !strings.Contains(text, "output \""+output+"\"") {
				t.Errorf("%s does not publish %s", file, output)
			}
		}
	}
}

func TestTerraformCreatesAdministratorCredential(t *testing.T) {
	cases := []struct {
		main, outputs, objectResource string
	}{
		{"../terraform/aws/main.tf", "../terraform/aws/outputs.tf", "aws_s3_object.admin_credential"},
		{"../terraform/gcp/main.tf", "../terraform/gcp/outputs.tf", "google_storage_bucket_object.admin_credential"},
	}
	for _, tc := range cases {
		mainRaw, err := os.ReadFile(tc.main)
		if err != nil {
			t.Fatal(err)
		}
		main := string(mainRaw)
		for _, required := range []string{
			`source = "hashicorp/random"`,
			`resource "random_bytes" "admin_credential"`,
			"length = 32",
			`random_bytes.admin_credential.base64`,
			`trimsuffix(random_bytes.admin_credential.base64, "=")`,
			`"+", "-"`,
			`"/", "_"`,
			`admin/credential.json`,
			`secret_digest = sha256(local.admin_credential)`,
			tc.objectResource,
		} {
			if !strings.Contains(main, required) {
				t.Errorf("%s does not contain %q", tc.main, required)
			}
		}
		if strings.Contains(main, `resource "random_id" "admin_credential"`) {
			t.Errorf("%s uses non-sensitive random_id for the administrator credential", tc.main)
		}

		outputsRaw, err := os.ReadFile(tc.outputs)
		if err != nil {
			t.Fatal(err)
		}
		outputs := string(outputsRaw)
		adminCredential := strings.Index(outputs, `output "admin_credential"`)
		if adminCredential < 0 || !strings.Contains(outputs[adminCredential:], "sensitive = true") {
			t.Errorf("%s must mark admin_credential sensitive", tc.outputs)
		}
		if !strings.Contains(outputs, `/admin/`) {
			t.Errorf("%s must publish the administration UI URL", tc.outputs)
		}
	}
}

func TestTerraformScopesMultiTenantControlAndWriteOnlyData(t *testing.T) {
	awsRaw, _ := os.ReadFile("../terraform/aws/main.tf")
	aws := string(awsRaw)
	for _, required := range []string{`v1/organization=*/control/`, `v1/organization=*/install=`, `v1/control/admin/credential.json`} {
		if !strings.Contains(aws, required) {
			t.Errorf("AWS policy lacks %q", required)
		}
	}
	if strings.Contains(aws, `Resource = "${aws_s3_bucket.fleet.arn}/*"`) {
		t.Fatal("AWS runtime can access every object")
	}
	// The install root is write-only apart from the one name, so the read grant must end at it.
	if !strings.Contains(aws, `Resource = "${aws_s3_bucket.fleet.arn}/${local.data_prefix}*/tags.json"`) {
		t.Error("AWS policy does not read install names from the one object name")
	}
	// Without a listing of that key S3 reports an absent name as 403, and the first naming fails.
	if !strings.Contains(aws, `Condition = { StringLike = { "s3:prefix" = "${local.data_prefix}*/tags.json" } }`) {
		t.Error("AWS policy cannot tell an absent install name from a denied one")
	}
	if strings.Contains(aws, `"s3:GetObject"`) && strings.Contains(aws, `Action   = "s3:GetObject"
        Resource = "${aws_s3_bucket.fleet.arn}/${local.data_prefix}*"`) {
		t.Fatal("AWS runtime can read every trajectory object")
	}

	gcpRaw, _ := os.ReadFile("../terraform/gcp/main.tf")
	gcp := string(gcpRaw)
	for _, required := range []string{`/control/.*`, `/install=[0-9a-f-]{36}/.*`, `v1/control/admin/credential.json`} {
		if !strings.Contains(gcp, required) {
			t.Errorf("GCP condition lacks %q", required)
		}
	}
	if strings.Contains(gcp, `resource.name.startsWith(local.object_root)`) {
		t.Fatal("GCP runtime can access every object")
	}
	if !strings.Contains(gcp, `/install=[0-9a-f-]{36}/tags\\.json$`) {
		t.Error("GCP condition does not read install names from the one object name")
	}
	if strings.Contains(gcp, `permissions = ["storage.objects.create", "storage.objects.delete", "storage.objects.get"]`) {
		t.Fatal("GCP data role can read every trajectory object")
	}

	for _, file := range []string{"../terraform/aws/main.tf", "../terraform/gcp/main.tf"} {
		raw, _ := os.ReadFile(file)
		if strings.Contains(string(raw), "FLEET_MANAGER_ORG") {
			t.Errorf("%s configures a single-organization runtime", file)
		}
	}
}

func TestDockerfileSeparatesLambdaAndCloudRunRuntimeUsers(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	lambda := strings.Index(text, "FROM gcr.io/distroless/static-debian12 AS lambda")
	cloudRun := strings.Index(text, "FROM gcr.io/distroless/static-debian12:nonroot AS cloud-run")
	if lambda < 0 || cloudRun <= lambda {
		t.Fatal("Dockerfile must keep the Lambda target before the default Cloud Run target")
	}
	if strings.Contains(text[lambda:cloudRun], "\nUSER ") {
		t.Error("Lambda target must let Lambda supply its runtime user")
	}
	if !strings.Contains(text[cloudRun:], "\nUSER 65532:65532\n") {
		t.Error("Cloud Run target must run as the distroless non-root user")
	}
	// Checked in pieces rather than as one literal: the build line carries a version stamp now, so
	// pinning it verbatim would pin the line wrapping too. Each piece still says something.
	for _, required := range []string{"go build -trimpath", "-s -w", "-X main.version=", "-o /out/fleet-manager ./src"} {
		if !strings.Contains(text, required) {
			t.Errorf("Dockerfile build line does not contain %q", required)
		}
	}
	// An unstamped image reports "dev", which is indistinguishable from a developer's local build.
	if !strings.Contains(text, "ARG VERSION") {
		t.Error("Dockerfile must take VERSION as a build argument")
	}
}

// The reporter credential is provisioned the same way and by the same mechanism, in both modules:
// it is what keeps "may report health" from meaning "may revoke your fleet".
func TestTerraformCreatesReporterCredential(t *testing.T) {
	for _, tc := range []struct {
		main, outputs, objectResource string
	}{
		{"../terraform/aws/main.tf", "../terraform/aws/outputs.tf", `resource "aws_s3_object" "reporter_credential"`},
		{"../terraform/gcp/main.tf", "../terraform/gcp/outputs.tf", `resource "google_storage_bucket_object" "reporter_credential"`},
	} {
		mainRaw, err := os.ReadFile(tc.main)
		if err != nil {
			t.Fatal(err)
		}
		main := string(mainRaw)
		for _, required := range []string{
			`resource "random_bytes" "reporter_credential"`,
			`trimsuffix(random_bytes.reporter_credential.base64, "=")`,
			`"fmr1.`,
			`reporter/credential.json`,
			`secret_digest = sha256(local.reporter_credential)`,
			tc.objectResource,
		} {
			if !strings.Contains(main, required) {
				t.Errorf("%s does not contain %q", tc.main, required)
			}
		}
		// Distinct random material: sharing it would make one credential two names for the same
		// secret, and rotating either would rotate both.
		if strings.Contains(main, `sha256(local.admin_credential)`) && strings.Count(main, "random_bytes") < 2 {
			t.Errorf("%s derives both credentials from one random resource", tc.main)
		}

		outputsRaw, err := os.ReadFile(tc.outputs)
		if err != nil {
			t.Fatal(err)
		}
		outputs := string(outputsRaw)
		at := strings.Index(outputs, `output "reporter_credential"`)
		if at < 0 {
			t.Errorf("%s does not publish reporter_credential", tc.outputs)
			continue
		}
		if !strings.Contains(outputs[at:at+120], "sensitive = true") {
			t.Errorf("%s must mark reporter_credential sensitive", tc.outputs)
		}
	}
}

func TestTerraformGrantsIdentityObjectAndExportsPublicKey(t *testing.T) {
	for _, provider := range []string{"aws", "gcp"} {
		base := "../terraform/" + provider + "/"
		main, err := os.ReadFile(base + "main.tf")
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{telemetryIdentityKey, `data "http" "fleet_manager_public_key"`, "/v1/telemetry/public-key", "local.telemetry_identity_key"} {
			if !strings.Contains(string(main), required) {
				t.Errorf("%s lacks %s", provider, required)
			}
		}
		outputs, err := os.ReadFile(base + "outputs.tf")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(outputs), `output "fleet_manager_public_key"`) || !strings.Contains(string(outputs), "jsondecode(data.http.fleet_manager_public_key.response_body).public_key") {
			t.Errorf("%s does not output the provisioned public key", provider)
		}
	}
}

// A deployment that changes nothing seals to Quesma's recipient, so history stays open to analytics
// the organization may ask for later, and forwards telemetry nowhere. Both defaults are pinned, and
// each template passes them to the service by the names it reads.
func TestTerraformOrganizationDefaults(t *testing.T) {
	for _, module := range []string{"aws", "gcp"} {
		read := func(name string) string {
			t.Helper()
			raw, err := os.ReadFile("../terraform/" + module + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		vars, main := read("variables.tf"), read("main.tf")
		for _, pinned := range []string{
			"variable \"default_allow_quesma_etl\" {\n  type        = bool\n  default     = true\n",
			"variable \"default_telemetry_collector_url\" {\n  type        = string\n  default     = \"\"\n",
		} {
			if !strings.Contains(vars, pinned) {
				t.Errorf("%s/variables.tf does not contain %q", module, pinned)
			}
		}
		for _, env := range []string{"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL", "FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL"} {
			if !strings.Contains(main, "\""+env+"\"") {
				t.Errorf("%s/main.tf does not pass %s", module, env)
			}
		}
	}
}
