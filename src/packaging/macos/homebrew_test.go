//go:build darwin

package macos

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// The macOS dispatch for a Homebrew install: outside an app bundle, ApplyTarget swaps the binary the
// cask's symlink points at, and the restart runs the marked build. Marker and restart check as in
// common.TestReExecAfterUpdate.
func TestRawExecutableUpdateReexec(t *testing.T) {
	marker := []byte("quesma-test-update")
	switch os.Getenv("QUESMA_TEST_BREW_REEXEC") {
	case "update":
		release := common.Release{Targets: map[string]string{"darwin/" + runtime.GOARCH: "raw-binary", "darwin/pkg": "package"}}
		if got := UpdateTarget(release); got != "raw-binary" {
			t.Fatalf("update target = %q", got)
		}
		raw, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyTarget(append(raw, marker...), "1.0.1"); err != nil {
			t.Fatal(err)
		}
		os.Setenv("QUESMA_TEST_BREW_REEXEC", "restarted")
		t.Fatal(common.ReExec())
	case "restarted":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if running, err := os.ReadFile(exe); err != nil || !bytes.HasSuffix(running, marker) {
			t.Fatalf("restarted the old binary: %v", err)
		}
		fmt.Println("restarted")
		return
	}
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	installed := filepath.Join(root, "Caskroom", "quesma-shipper", "1.0.0", "quesma-shipper")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shipper")
	if err := os.Symlink(installed, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "-test.run=^TestRawExecutableUpdateReexec$")
	cmd.Env = append(os.Environ(), "QUESMA_TEST_BREW_REEXEC=update")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "restarted\n") {
		t.Fatalf("update and re-exec: %v, %s", err, out)
	}
	if target, err := os.Readlink(link); err != nil || target != installed {
		t.Fatalf("command link changed: %q, %v", target, err)
	}
}

func TestHomebrewServiceOwnership(t *testing.T) {
	brew := "/opt/brew & tools/Caskroom/quesma-shipper/1.0.0/quesma-shipper"
	upgrade := "/opt/brew & tools/Caskroom/quesma-shipper/1.0.1/quesma-shipper"
	native := "/Users/me/Applications/Quesma Shipper.app/Contents/MacOS/quesma-shipper"
	for _, tc := range []struct {
		name, previous, next string
		allowed              bool
	}{
		{"fresh install", "", brew, true},
		{"brew reinstall", brew, brew, true},
		{"brew upgrade", brew, upgrade, true},
		{"native reinstall", native, native, true},
		{"native to brew needs removal", native, brew, false},
		{"brew to native needs removal", brew, native, false},
		{"another brew prefix", brew, "/usr/local/Caskroom/quesma-shipper/1.0.1/quesma-shipper", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), "agent.plist")
			if tc.previous != "" {
				spec := testSpec()
				spec.Executable = tc.previous
				if err := os.WriteFile(plist, []byte(renderPlist(spec)), 0o600); err != nil {
					t.Fatal(err)
				}
				if got := common.ServiceProgram(Status{Path: plist}); got != tc.previous {
					t.Fatalf("program = %q", got)
				}
			}
			if err := checkInstallOwner(tc.next, plist); (err == nil) != tc.allowed {
				t.Fatalf("checkInstallOwner = %v, allowed = %v", err, tc.allowed)
			}
		})
	}
}

func TestHomebrewProgramCannotRemoveItself(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "Caskroom", "quesma-shipper", "1.0.0", "quesma-shipper")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("installed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveProgram(exe); err == nil || !strings.Contains(err.Error(), "brew uninstall") {
		t.Fatalf("RemoveProgram = %v", err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatal(err)
	}
}

func TestHomebrewUninstallOwnership(t *testing.T) {
	exe := "/opt/homebrew/Caskroom/quesma-shipper/1.0.0/quesma-shipper"
	for _, tc := range []struct {
		name, program string
		owned, bad    bool
	}{
		{"current cask", exe, true, false},
		{"replacement cask", "/opt/homebrew/Caskroom/quesma-shipper/1.0.1/quesma-shipper", false, false},
		{"native installation", "/Users/me/Applications/Quesma Shipper.app/Contents/MacOS/quesma-shipper", false, false},
		{"missing entry", "", false, false},
		{"invalid entry", "invalid", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), "agent.plist")
			if tc.program != "" {
				spec := testSpec()
				spec.Executable = tc.program
				raw := renderPlist(spec)
				if tc.bad {
					raw = "broken plist"
				}
				if err := os.WriteFile(plist, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			owned, err := ownsHomebrewService(exe, plist)
			if owned != tc.owned || (err != nil) != tc.bad {
				t.Fatalf("ownership = %v, %v", owned, err)
			}
		})
	}
}
