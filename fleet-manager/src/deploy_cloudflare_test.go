package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testDigest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

// deployCloudflare runs `deploy.sh cloudflare <args>` against stand-ins for wrangler, docker and
// curl. wrangler records each call, what it read on stdin and the secrets file it was handed.
func deployCloudflare(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("deploy.sh is a POSIX shell script")
	}
	for _, sub := range []string{"bin", "home", "state", "log", "template/fleet-manager/cloudflare/src"} {
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
	for _, name := range []string{"wrangler.jsonc", "Dockerfile", "src/index.js"} {
		raw, err := os.ReadFile(filepath.Join("../cloudflare", name))
		if err != nil {
			t.Fatal(err)
		}
		write("template/fleet-manager/cloudflare/"+name, string(raw), 0o644)
	}
	log := filepath.Join(dir, "log")
	write("bin/wrangler", `#!/bin/sh
echo "$*" >>"`+log+`/wrangler-calls"
case "$1 $2 $3" in
"r2 bucket info") exit 1 ;;
"r2 object put") cat >"`+log+`/object" ;;
esac
if [ "$1" = deploy ]; then
	while [ $# -gt 0 ]; do [ "$1" = --secrets-file ] && cp "$2" "`+log+`/secrets.json"; shift; done
	echo "Deployed fleet-manager https://fleet-manager.example.workers.dev"
fi
exit 0
`, 0o755)
	write("bin/docker", `#!/bin/sh
[ "$1 $2 $3" = "buildx imagetools inspect" ] && echo '"`+testDigest+`"'
exit 0
`, 0o755)
	write("bin/curl", `#!/bin/sh
case "$*" in *codeload*) exec tar -czf - -C '`+filepath.Join(dir, "template")+`' . ;; esac
exit 0
`, 0o755)
	cmd := exec.Command("sh", append([]string{"../deploy.sh", "cloudflare"}, append(args, "--dir", filepath.Join(dir, "state"))...)...)
	cmd.Env = append([]string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"),
		"WRANGLER=" + filepath.Join(dir, "bin", "wrangler")}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestDeployCloudflareRefusesAFirstRunWithoutR2Keys(t *testing.T) {
	dir := t.TempDir()
	out, err := deployCloudflare(t, dir, []string{"CLOUDFLARE_ACCOUNT_ID=" + testR2Account, "FLEET_MANAGER_R2_ACCESS_KEY_ID=k"}, "--bucket", "fleet")
	if err == nil || !strings.Contains(out, "FLEET_MANAGER_R2_SECRET_ACCESS_KEY") {
		t.Fatalf("deployed without both R2 keys: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "log/wrangler-calls")); err == nil {
		t.Error("wrangler ran before the keys were checked")
	}
}

func TestDeployCloudflareUploadsOnlyTheDigestAndKeepsKeysOutOfState(t *testing.T) {
	dir := t.TempDir()
	keys := []string{"CLOUDFLARE_ACCOUNT_ID=" + testR2Account, "FLEET_MANAGER_R2_ACCESS_KEY_ID=key-id", "FLEET_MANAGER_R2_SECRET_ACCESS_KEY=key-secret"}
	out, err := deployCloudflare(t, dir, keys, "--bucket", "fleet")
	if err != nil {
		t.Fatalf("deploy.sh: %v\n%s", err, out)
	}
	state := filepath.Join(dir, "state/cloudflare")
	credential, err := os.ReadFile(filepath.Join(state, "admin_credential"))
	if err != nil || !strings.HasPrefix(string(credential), "fma1.") {
		t.Fatalf("admin credential %q: %v", credential, err)
	}
	if info, _ := os.Stat(filepath.Join(state, "admin_credential")); info.Mode().Perm() != 0o600 {
		t.Errorf("admin credential mode %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(out, string(credential)) || !strings.Contains(out, "https://fleet-manager.example.workers.dev/admin/") {
		t.Errorf("output names neither the credential nor the admin URL:\n%s", out)
	}
	sum := sha256.Sum256(credential)
	if object, _ := os.ReadFile(filepath.Join(dir, "log/object")); string(object) != `{"schema":1,"secret_digest":"`+hex.EncodeToString(sum[:])+`"}` {
		t.Errorf("uploaded %q, want only the credential's digest", object)
	}
	dockerfile, _ := os.ReadFile(filepath.Join(state, "worker/Dockerfile"))
	if !strings.Contains(string(dockerfile), "FROM docker.io/quesma/fleet-manager@"+testDigest+"\n") {
		t.Errorf("image is not pinned to the resolved digest:\n%s", dockerfile)
	}
	var secrets map[string]string
	raw, _ := os.ReadFile(filepath.Join(dir, "log/secrets.json"))
	if err := json.Unmarshal(raw, &secrets); err != nil {
		t.Fatalf("secrets file %q: %v", raw, err)
	}
	want := map[string]string{"FLEET_MANAGER_BUCKET": "fleet", "FLEET_MANAGER_ACCOUNT": testR2Account, "FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL": "false",
		"FLEET_MANAGER_R2_ACCESS_KEY_ID": "key-id", "FLEET_MANAGER_R2_SECRET_ACCESS_KEY": "key-secret"}
	if len(secrets) != len(want) {
		t.Errorf("secrets %v, want %v", secrets, want)
	}
	for name, value := range want {
		if secrets[name] != value {
			t.Errorf("secret %s = %q, want %q", name, secrets[name], value)
		}
	}
	entries, _ := os.ReadDir(state)
	for _, entry := range entries {
		if raw, _ := os.ReadFile(filepath.Join(state, entry.Name())); strings.Contains(string(raw), "key-secret") {
			t.Errorf("%s keeps the R2 secret on disk", entry.Name())
		}
	}

	// An upgrade needs no keys: Cloudflare keeps the stored secrets and the credential is reused.
	out, err = deployCloudflare(t, dir, []string{"CLOUDFLARE_ACCOUNT_ID=" + testR2Account})
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	again, _ := os.ReadFile(filepath.Join(state, "admin_credential"))
	if string(again) != string(credential) {
		t.Error("an upgrade minted a new admin credential")
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "log/secrets.json"))
	if strings.Contains(string(raw), "R2_ACCESS_KEY") || !strings.Contains(string(raw), `"FLEET_MANAGER_BUCKET":"fleet"`) {
		t.Errorf("second run secrets %s, want the remembered bucket and no R2 keys", raw)
	}
}

// The Worker forwards exactly what deploy.sh stores, so a setting cannot be stored and then dropped.
func TestCloudflareWorkerForwardsWhatDeployStores(t *testing.T) {
	worker, err := os.ReadFile("../cloudflare/src/index.js")
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := os.ReadFile("../deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"FLEET_MANAGER_BUCKET", "FLEET_MANAGER_ACCOUNT", "FLEET_MANAGER_R2_JURISDICTION", "FLEET_MANAGER_R2_ACCESS_KEY_ID",
		"FLEET_MANAGER_R2_SECRET_ACCESS_KEY", "FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL", "FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL"} {
		if !strings.Contains(string(worker), `"`+name+`"`) || !strings.Contains(string(deploy), `"`+name+`":`) {
			t.Errorf("%s is not both stored by deploy.sh and forwarded by the Worker", name)
		}
	}
	for _, want := range []string{`FLEET_MANAGER_PROVIDER: "r2"`, "/healthz", "enableInternet: true", "container.inspect()"} {
		if !strings.Contains(string(worker), want) {
			t.Errorf("Worker lacks %s", want)
		}
	}
}
