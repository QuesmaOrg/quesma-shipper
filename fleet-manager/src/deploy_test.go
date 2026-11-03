package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

// deployAnalyticsKey runs `deploy.sh aws analytics-key <args>` against stand-ins for terraform,
// age-keygen and aws, which record what they were given. cliHistory is what the AWS CLI's
// `cli_history` setting reads.
func deployAnalyticsKey(t *testing.T, keyARN, cliHistory string, args ...string) (dir, stdout, stderr string, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("deploy.sh is a POSIX shell script")
	}
	identity, genErr := age.GenerateX25519Identity()
	if genErr != nil {
		t.Fatal(genErr)
	}
	dir = t.TempDir()
	for _, sub := range []string{"bin", "home", "state/aws", "log"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "state/aws/terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("terraform", `[ "$4" = analytics_key_arn ] && printf '%s' '`+keyARN+`'`+"\n")
	stub("age-keygen", `touch "`+dir+`/log/age-keygen"
if [ "${1:-}" = -y ]; then grep -qx '`+identity.String()+`' && echo '`+identity.Recipient().String()+`'; exit; fi
printf '# created: 2026-10-27\n# public key: `+identity.Recipient().String()+`\n`+identity.String()+`\n'
`)
	stub("aws", `[ "$1" = configure ] && { [ -z '`+cliHistory+`' ] || echo '`+cliHistory+`'; exit 0; }
printf '%s\n' "$@" >"`+dir+`/log/aws-argv"; cat >"`+dir+`/log/aws-stdin"; echo AQIDAHhkbXNjaXBoZXJ0ZXh0`+"\n")

	cmd := exec.Command("sh", append([]string{"../deploy.sh", "aws", "analytics-key"}, append(args, "--dir", filepath.Join(dir, "state"))...)...)
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"),
		"TF=" + filepath.Join(dir, "bin", "terraform")}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if err == nil {
		stdin, _ := os.ReadFile(filepath.Join(dir, "log/aws-stdin"))
		if string(stdin) != identity.String()+"\n" {
			t.Errorf("aws kms encrypt read %q on stdin, want the identity line", stdin)
		}
		var printed AnalyticsIdentity
		if jsonErr := json.Unmarshal([]byte(out.String()), &printed); jsonErr != nil || printed.Recipient != identity.Recipient().String() || printed.Organization != args[0] {
			t.Errorf("printed %q (%v), want the organization and the identity's recipient", out.String(), jsonErr)
		}
	}
	return dir, out.String(), errOut.String(), err
}

// The identity reaches KMS on stdin and nowhere else: not in an argument, not in a file, not in
// what the command prints. What it prints is what the organization form takes.
func TestDeployAnalyticsKeyPrintsOnlyTheRecipientAndCiphertext(t *testing.T) {
	dir, stdout, stderr, err := deployAnalyticsKey(t, testAnalyticsKeyARN, "", "globex")
	if err != nil {
		t.Fatalf("deploy.sh: %v\n%s", err, stderr)
	}
	var printed AnalyticsIdentity
	if err := json.Unmarshal([]byte(stdout), &printed); err != nil {
		t.Fatal(err)
	}
	printed.Recipient = twoRecipients(t)[0]
	if err := validateConfig(FleetConfig{Schema: schemaVersion, Organization: "globex", IncludeInstallRecipient: true, AnalyticsIdentity: &printed}); err != nil {
		t.Errorf("Fleet Manager refuses what deploy.sh printed: %v", err)
	}
	if strings.Contains(stdout+stderr, "AGE-SECRET-KEY-") {
		t.Error("the private identity was printed")
	}

	argv, _ := os.ReadFile(filepath.Join(dir, "log/aws-argv"))
	for _, want := range []string{"kms\nencrypt\n", "--key-id\n" + testAnalyticsKeyARN + "\n", "--region\neu-central-1\n",
		"--plaintext\nfileb:///dev/stdin\n", "--encryption-context\npurpose=analytics-identity,organization=globex\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("aws was called with\n%s\nwhich lacks %q", argv, want)
		}
	}

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Dir(path) == filepath.Join(dir, "bin") || path == filepath.Join(dir, "log/aws-stdin") {
			return err
		}
		if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "AGE-SECRET-KEY-") {
			t.Errorf("%s holds the private identity", path)
		}
		return nil
	})
}

func TestDeployAnalyticsKeyRefusesWithoutAKeyOrAnOrganization(t *testing.T) {
	for name, tc := range map[string]struct {
		key  string
		args []string
		want string
	}{
		"no analytics key deployed": {"", []string{"acme"}, "--analytics-key"},
		"no organization":           {testAnalyticsKeyARN, nil, "organization slug"},
		"not a slug":                {testAnalyticsKeyARN, []string{"Acme,organization=globex"}, "organization slug"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _, stderr, err := deployAnalyticsKey(t, tc.key, "", tc.args...)
			if err == nil || !strings.Contains(stderr, tc.want) {
				t.Errorf("err = %v, stderr %q; want a refusal mentioning %q", err, stderr, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "log/aws-argv")); statErr == nil {
				t.Error("aws was called anyway")
			}
		})
	}
}

// The CLI's history would keep the identity in its own database, so the command stops before one
// exists.
func TestDeployAnalyticsKeyRefusesWhileTheCLIHistoryIsOn(t *testing.T) {
	dir, _, stderr, err := deployAnalyticsKey(t, testAnalyticsKeyARN, "enabled", "acme")
	if err == nil || !strings.Contains(stderr, "cli_history") {
		t.Fatalf("err = %v, stderr %q; want a refusal naming cli_history", err, stderr)
	}
	for _, ran := range []string{"log/age-keygen", "log/aws-argv"} {
		if _, statErr := os.Stat(filepath.Join(dir, ran)); statErr == nil {
			t.Errorf("%s ran anyway", filepath.Base(ran))
		}
	}
}

// Terraform would schedule the analytics key's deletion while it refuses the bucket, so destroy
// stops first while any version is left.
func TestDeployDestroyKeepsTheAnalyticsKeyWhileTheBucketHoldsObjects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy.sh is a POSIX shell script")
	}
	for name, tc := range map[string]struct {
		key, left string
		destroys  bool
	}{
		"objects left":        {testAnalyticsKeyARN, "1\t0", false},
		"delete markers left": {testAnalyticsKeyARN, "0\t1", false},
		"empty bucket":        {testAnalyticsKeyARN, "0\t0", true},
		"no analytics key":    {"", "1\t0", true},
		"bucket already gone": {testAnalyticsKeyARN, "NoSuchBucket", true},
		"listing refused":     {testAnalyticsKeyARN, "AccessDenied", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"bin", "home", "state/aws", "log"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "state/aws/terraform.tfstate"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			stub := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("terraform", `for a; do [ "$a" = destroy ] && touch "`+dir+`/log/destroyed"; done
[ "$4" = analytics_key_arn ] && printf '%s' '`+tc.key+`'
[ "$4" = bucket ] && printf fleet
exit 0
`)
			stub("aws", `case '`+tc.left+`' in NoSuchBucket | AccessDenied) echo "An error occurred (`+tc.left+`) when calling the ListObjectVersions operation" >&2; exit 254 ;; esac
echo "a warning the CLI printed" >&2
printf '`+tc.left+`\n'`+"\n")
			cmd := exec.Command("sh", "../deploy.sh", "aws", "destroy", "-y", "--dir", filepath.Join(dir, "state"))
			cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"),
				"TF=" + filepath.Join(dir, "bin", "terraform")}
			var errOut strings.Builder
			cmd.Stderr = &errOut
			err := cmd.Run()
			_, statErr := os.Stat(filepath.Join(dir, "log/destroyed"))
			if destroyed := statErr == nil; destroyed != tc.destroys {
				t.Errorf("destroyed = %v, want %v (err %v, stderr %q)", destroyed, tc.destroys, err, errOut.String())
			}
			if !tc.destroys && (err == nil || !strings.Contains(errOut.String(), "the bucket still holds objects")) {
				t.Errorf("err = %v, stderr %q; want a refusal about the bucket's objects", err, errOut.String())
			}
		})
	}
}

// A new deployment states Quesma's recipient off. One made before states nothing, as an older copy
// of deploy.sh does, so the template's default (on, TestTerraformOrganizationDefaults) keeps what it
// had: an upgrade never turns the recipient off.
func TestDeployStatesQuesmasRecipientOffForANewDeployment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy.sh is a POSIX shell script")
	}
	for name, tc := range map[string]struct {
		deployed bool
		stored   string
		args     []string
		want     string
		again    bool // run once more after the first run deployed
	}{
		"new":                  {false, "", nil, "false", false},
		"new, then run again":  {false, "", nil, "false", true},
		"existing":             {true, "", nil, "", false},
		"existing, told off":   {true, "", []string{"--no-quesma-etl"}, "false", false},
		"existing, stored off": {true, "false", nil, "false", false},
		"existing, stored on":  {true, "true", nil, "true", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"bin", "home", "state/aws", "template/fleet-manager/terraform/aws"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, body string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			write("template/fleet-manager/terraform/aws/main.tf", "", 0o644)
			if tc.deployed {
				write("state/aws/terraform.tfstate", "{}", 0o600)
			}
			if tc.stored != "" {
				write("state/aws/terraform.tfvars.json", "{\n  \"bucket\": \"fleet\",\n  \"region\": \"eu-central-1\",\n  \"default_allow_quesma_etl\": "+tc.stored+"\n}\n", 0o600)
			}
			write("bin/terraform", "#!/bin/sh\nexit 0\n", 0o755)
			write("bin/aws", "#!/bin/sh\ncase $1 in sts) echo '111122223333 arn:aws:iam::111122223333:user/admin' ;; ec2) echo vpc-1 ;; esac\n", 0o755)
			write("bin/curl", "#!/bin/sh\nexec tar -czf - -C '"+filepath.Join(dir, "template")+"' .\n", 0o755)

			args := append([]string{"../deploy.sh", "aws", "plan", "--bucket", "fleet", "--region", "eu-central-1", "--dir", filepath.Join(dir, "state")}, tc.args...)
			cmd := exec.Command("sh", args...)
			cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"),
				"TF=" + filepath.Join(dir, "bin", "terraform")}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("deploy.sh: %v\n%s", err, out)
			}
			if tc.again {
				write("state/aws/terraform.tfstate", "{}", 0o600)
				again := exec.Command("sh", args...)
				again.Env = cmd.Env
				if out, err = again.CombinedOutput(); err != nil {
					t.Fatalf("deploy.sh, second run: %v\n%s", err, out)
				}
			}
			vars, err := os.ReadFile(filepath.Join(dir, "state/aws/terraform.tfvars.json"))
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if m := regexp.MustCompile(`"default_allow_quesma_etl": (\w+)`).FindSubmatch(vars); m != nil {
				got = string(m[1])
			}
			if got != tc.want {
				t.Errorf("default_allow_quesma_etl = %q, want %q:\n%s", got, tc.want, vars)
			}
		})
	}
}
