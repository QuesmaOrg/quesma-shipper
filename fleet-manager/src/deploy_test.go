package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

// deployAnalyticsKey runs `deploy.sh aws analytics-key <args>` against stand-ins for terraform,
// age-keygen and aws, which record what they were given.
func deployAnalyticsKey(t *testing.T, keyARN string, args ...string) (dir, stdout, stderr string, err error) {
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
	stub("age-keygen", `if [ "${1:-}" = -y ]; then grep -qx '`+identity.String()+`' && echo '`+identity.Recipient().String()+`'; exit; fi
printf '# created: 2026-10-27\n# public key: `+identity.Recipient().String()+`\n`+identity.String()+`\n'
`)
	stub("aws", `printf '%s\n' "$@" >"`+dir+`/log/aws-argv"; cat >"`+dir+`/log/aws-stdin"; echo AQIDAHhkbXNjaXBoZXJ0ZXh0`+"\n")

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
		if jsonErr := json.Unmarshal([]byte(out.String()), &printed); jsonErr != nil || printed.Recipient != identity.Recipient().String() {
			t.Errorf("printed %q (%v), want the identity's recipient", out.String(), jsonErr)
		}
	}
	return dir, out.String(), errOut.String(), err
}

// The identity reaches KMS on stdin and nowhere else: not in an argument, not in a file, not in
// what the command prints. What it prints is what the organization form takes.
func TestDeployAnalyticsKeyPrintsOnlyTheRecipientAndCiphertext(t *testing.T) {
	dir, stdout, stderr, err := deployAnalyticsKey(t, testAnalyticsKeyARN, "globex")
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
			dir, _, stderr, err := deployAnalyticsKey(t, tc.key, tc.args...)
			if err == nil || !strings.Contains(stderr, tc.want) {
				t.Errorf("err = %v, stderr %q; want a refusal mentioning %q", err, stderr, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "log/aws-argv")); statErr == nil {
				t.Error("aws was called anyway")
			}
		})
	}
}
