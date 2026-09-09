// install.sh against a stub shipper that records its argv: the script's job is wiring, and the
// verbs it calls are tested on their own.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const stubShipper = `#!/bin/sh
echo "$@" >> "$SHIPPER_STUB_LOG"
[ "$1" = --version ] && echo "quesma-shipper 0.0.0-stub"
exit 0
`

type installWorld struct{ *world }

func stageInstall(t *testing.T) installWorld {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is unix-only")
	}
	return installWorld{world: stageBareWorld(t)}
}

func (w installWorld) bin() string     { return filepath.Join(w.Home, "bin") }
func (w installWorld) shipper() string { return filepath.Join(w.bin(), "quesma-shipper") }
func (w installWorld) stubLog() string { return filepath.Join(w.Home, "stub.log") }

func stageStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shipper-local")
	if err := os.WriteFile(path, []byte(stubShipper), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runInstall(t *testing.T, w installWorld, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "packaging", "linux", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script, "--bin-dir", w.bin()}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + w.Home,
		"XDG_STATE_HOME=" + w.State,
		"SHIPPER_STUB_LOG=" + w.stubLog(),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func stubCalls(t *testing.T, w installWorld) []string {
	t.Helper()
	raw, err := os.ReadFile(w.stubLog())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func markEnrolled(t *testing.T, w installWorld) {
	t.Helper()
	if err := os.MkdirAll(statePath(w.world), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath(w.world), "enrollment.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstallScriptPlacesALocalBinaryAndLogsIn(t *testing.T) {
	w := stageInstall(t)
	out, err := runInstall(t, w, "--from", stageStub(t), "inv-1", "--server", "http://cp.example")
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if calls := stubCalls(t, w); !slices.Equal(calls, []string{
		"--version",
		"login --server http://cp.example inv-1",
		"postinstall",
	}) {
		t.Errorf("stub calls = %v\n%s", calls, out)
	}
	if placed, err := os.ReadFile(w.shipper()); err != nil || string(placed) != stubShipper {
		t.Fatalf("binary not placed: %v\n%s", err, out)
	}
}

func TestInstallScriptKeepsAnExistingLogin(t *testing.T) {
	w := stageInstall(t)
	markEnrolled(t, w)
	out, err := runInstall(t, w, "--from", stageStub(t), "--no-service")
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already logged in") {
		t.Errorf("missing existing-login message:\n%s", out)
	}
	if calls := stubCalls(t, w); !slices.Equal(calls, []string{"--version"}) {
		t.Errorf("stub calls = %v", calls)
	}
}

func TestInstallScriptDoesNotRequireEnrollment(t *testing.T) {
	w := stageInstall(t)
	out, err := runInstall(t, w, "--from", stageStub(t), "--no-service")
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not enrolled") {
		t.Errorf("missing enrollment instructions:\n%s", out)
	}
	if calls := stubCalls(t, w); !slices.Equal(calls, []string{"--version"}) {
		t.Errorf("stub calls = %v", calls)
	}
}

func TestInstallScriptRequiresServerBeforeChangingAnything(t *testing.T) {
	w := stageInstall(t)
	out, err := runInstall(t, w, "--from", stageStub(t), "token", "--no-service")
	if err == nil || !strings.Contains(out, "pass --server URL") {
		t.Fatalf("first install without --server was not refused: %v\n%s", err, out)
	}
	if _, err := os.Stat(w.shipper()); !os.IsNotExist(err) {
		t.Errorf("destination changed: %v", err)
	}
	if calls := stubCalls(t, w); calls != nil {
		t.Errorf("stub was called: %v", calls)
	}
}

func TestInstallScriptChecksLocalInputBeforeChangingAnything(t *testing.T) {
	w := stageInstall(t)
	out, err := runInstall(t, w, "--from", w.shipper()+".missing", "token", "--no-service")
	if err == nil || !strings.Contains(out, "no such file") {
		t.Fatalf("missing --from file was not refused: %v\n%s", err, out)
	}
	if _, err := os.Stat(w.shipper()); !os.IsNotExist(err) {
		t.Errorf("destination changed: %v", err)
	}
}

// --- rename bridge: delete this block with the retirement in install.sh -----------------------

func (w installWorld) legacy() string { return filepath.Join(w.bin(), "shipper") }

// stageLegacy puts an install at the former path in place, answering --version with banner.
func stageLegacy(t *testing.T, w installWorld, banner string) {
	t.Helper()
	if err := os.MkdirAll(w.bin(), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := strings.Replace(stubShipper, "quesma-shipper 0.0.0-stub", banner, 1)
	if err := os.WriteFile(w.legacy(), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
}

// An install at the former path answers with the old banner; one that already self-updated there
// answers with the new one. Both are this program, and a reinstall retires both.
func TestInstallScriptRetiresTheFormerBinaryOnceTheServiceIsRepointed(t *testing.T) {
	for _, banner := range []string{"shipper 0.0.0-stub", "quesma-shipper 0.0.0-stub"} {
		t.Run(banner, func(t *testing.T) {
			w := stageInstall(t)
			stageLegacy(t, w, banner)
			out, err := runInstall(t, w, "--from", stageStub(t))
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%s", err, out)
			}
			if _, err := os.Stat(w.legacy()); !os.IsNotExist(err) {
				t.Fatalf("former binary still exists: %v", err)
			}
			if _, err := os.Stat(w.shipper()); err != nil {
				t.Fatalf("renamed binary is missing: %v", err)
			}
		})
	}
}

// --no-service skips postinstall, so a service entry may still name the former path: removing the
// file it names would leave the entry pointing at nothing and stop collection silently.
func TestInstallScriptKeepsTheFormerBinaryWhenTheServiceIsNotRepointed(t *testing.T) {
	w := stageInstall(t)
	stageLegacy(t, w, "shipper 0.0.0-stub")
	out, err := runInstall(t, w, "--from", stageStub(t), "--no-service")
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(w.legacy()); err != nil {
		t.Fatalf("former binary was removed while a service entry may still name it: %v", err)
	}
	if !strings.Contains(out, "kept "+w.legacy()) {
		t.Errorf("keeping the former binary was not reported:\n%s", out)
	}
}

// The old name is not ours to claim: only a binary that identifies itself as this program goes.
func TestInstallScriptLeavesAnUnrelatedProgramWithTheFormerName(t *testing.T) {
	w := stageInstall(t)
	stageLegacy(t, w, "some-other-shipper 1.0")
	out, err := runInstall(t, w, "--from", stageStub(t))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(w.legacy()); err != nil {
		t.Fatalf("an unrelated program with the old name was removed: %v", err)
	}
}
