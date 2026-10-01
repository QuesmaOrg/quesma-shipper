//go:build darwin

package macos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

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
