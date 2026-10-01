//go:build darwin

package macos

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestHomebrewUpdateReexec(t *testing.T) {
	if os.Getenv("QUESMA_TEST_BREW_REEXEC") == "1" {
		release := common.Release{Targets: map[string]string{"darwin/" + runtime.GOARCH: "raw-binary", "darwin/pkg": "package"}}
		if got := UpdateTarget(release); got != "raw-binary" {
			t.Fatalf("update target = %q", got)
		}
		raw, err := os.ReadFile(os.Getenv("QUESMA_TEST_BREW_REPLACEMENT"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyTarget(raw, "1.0.1"); err != nil {
			t.Fatal(err)
		}
		os.Args = []string{os.Args[0], "self-update restarted"}
		t.Fatal(common.ReExec())
	}
	root := t.TempDir()
	helper := filepath.Join(root, "replacement.go")
	if err := os.WriteFile(helper, []byte("package main\nimport (\"fmt\"; \"os\")\nfunc main() { fmt.Println(os.Args[1]) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if out, err := exec.Command("go", "build", "-o", replacement, helper).CombinedOutput(); err != nil {
		t.Fatalf("build replacement: %v, %s", err, out)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
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
	plist := filepath.Join(root, "agent.plist")
	spec := testSpec()
	spec.Executable = installed
	if err := os.WriteFile(plist, []byte(renderPlist(spec)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "-test.run=^TestHomebrewUpdateReexec$")
	cmd.Env = append(os.Environ(), "QUESMA_TEST_BREW_REEXEC=1", "QUESMA_TEST_BREW_REPLACEMENT="+replacement)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "self-update restarted\n" {
		t.Fatalf("update and reexec: %v, %s", err, out)
	}
	if target, err := os.Readlink(link); err != nil || target != installed {
		t.Fatalf("command link changed: %q, %v", target, err)
	}
	if got := common.ServiceProgram(Status{Path: plist}); got != installed {
		t.Fatalf("service program changed: %q", got)
	}
	updated, err := os.ReadFile(installed)
	if err != nil || bytes.Equal(raw, updated) {
		t.Fatalf("binary was not replaced: %v", err)
	}
	if out, err := exec.Command(link, "next launch").CombinedOutput(); err != nil || string(out) != "next launch\n" {
		t.Fatalf("launch after update: %v, %s", err, out)
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
