//go:build darwin

package macos

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemRemovalPreservesPersonalInstallation(t *testing.T) {
	for _, tc := range []struct {
		name, owner                       string
		fields                            string
		printExit, bootoutExit            int
		missingPlist, wantStop, wantError bool
	}{
		{name: "loaded system with dormant personal plist", owner: "system", wantStop: true},
		{name: "loaded personal", owner: "personal"},
		{name: "loaded Homebrew", owner: "personal"},
		{name: "dormant personal", printExit: 113},
		{name: "logged out user", printExit: 112},
		{name: "unavailable home", owner: "personal"},
		{name: "unknown owner", owner: "unknown", wantError: true},
		{name: "submitted system job", fields: "\tprogram = " + installedExecutable("/") + "\n", wantStop: true},
		{name: "submitted personal job", fields: "\tprogram = /bin/sleep\n"},
		{name: "relative path cannot fall back", fields: "\tpath = relative.plist\n\tprogram = /bin/sleep\n", wantError: true},
		{name: "empty path cannot fall back", fields: "\tpath = \n\tprogram = /bin/sleep\n", wantError: true},
		{name: "duplicate path cannot fall back", fields: "\tpath = /a.plist\n\tpath = /b.plist\n\tprogram = /bin/sleep\n", wantError: true},
		{name: "duplicate program", fields: "\tprogram = /bin/sleep\n\tprogram = /bin/sleep\n", wantError: true},
		{name: "relative program", fields: "\tprogram = sleep\n", wantError: true},
		{name: "nested program", fields: "\tenvironment = {\n\t\tprogram = /bin/sleep\n\t}\n", wantError: true},
		{name: "neither field", fields: "\tstate = running\n", wantError: true},
		{name: "inspection denied", printExit: 1, wantError: true},
		{name: "system stop failed", owner: "system", bootoutExit: 1, wantStop: true, wantError: true},
		{name: "loaded system missing plist", owner: "system", missingPlist: true, wantError: true},
		{name: "loaded personal missing system plist", owner: "personal", missingPlist: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			exe := writeTestApp(t, installedApp(home), bundleIdentifier, executableName)
			if tc.name == "loaded Homebrew" {
				exe = "/opt/homebrew/Caskroom/quesma-shipper/0.0.3/quesma-shipper"
			}
			files := map[string]string{
				launchdPath(home): renderPlist(Spec{Executable: exe}),
				filepath.Join(home, ".local/state/trajectory-shipper/identity.json"): "synthetic enrollment",
				filepath.Join(home, ".local/state/trajectory-shipper/state.db"):      "synthetic upload history",
			}
			for path, contents := range files {
				writeUninstallFixture(t, path, contents, 0o600)
			}
			personalLink := filepath.Join(home, ".local/bin/quesma-shipper")
			if err := installCLILink(personalLink, exe); err != nil {
				t.Fatal(err)
			}
			previousPath, previousLaunchctl := systemAgentPath, launchctl
			systemAgentPath = filepath.Join(t.TempDir(), "system-agent.plist")
			launchctl = filepath.Join(t.TempDir(), "launchctl")
			t.Cleanup(func() { systemAgentPath, launchctl = previousPath, previousLaunchctl })
			if !tc.missingPlist {
				writeUninstallFixture(t, systemAgentPath, renderSystemPlist(), 0o644)
			}
			path := map[string]string{"system": systemAgentPath, "personal": launchdPath(home)}[tc.owner]
			fields := tc.fields
			if fields == "" {
				fields = "\tpath = " + path + "\n"
			}
			output := filepath.Join(t.TempDir(), "service-output")
			writeUninstallFixture(t, output, "gui/501/com.quesma.shipper = {\n"+fields+"}\n", 0o600)
			marker := filepath.Join(t.TempDir(), "bootout")
			calls := filepath.Join(t.TempDir(), "calls")
			fake := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
print)
  [ ! -e %q ] || exit 113
  [ %d = 0 ] || exit %d
  cat %q
  ;;
bootout)
  touch %q
  exit %d
  ;;
*) exit 99 ;;
esac
`, calls, marker, tc.printExit, tc.printExit, output, marker, tc.bootoutExit)
			writeUninstallFixture(t, launchctl, fake, 0o700)
			accountHome := home
			if tc.name == "unavailable home" {
				accountHome = "/unavailable/directory/account"
			}
			users := []localUser{{uid: "501", home: accountHome}, {uid: "501", home: accountHome + "/alias"}}
			err := preUninstallSystem(users)
			if (err != nil) != tc.wantError {
				t.Fatalf("preUninstallSystem = %v, want error %v", err, tc.wantError)
			}
			_, err = os.Stat(marker)
			if (err == nil) != tc.wantStop {
				t.Fatalf("bootout = %v, want %v", err, tc.wantStop)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(commands), "bootout "); n > 1 {
				t.Fatalf("duplicate account stopped %d times", n)
			}
			for path, want := range files {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("personal file %s changed: %v", path, err)
				}
			}
			if got, err := os.Readlink(personalLink); err != nil || got != exe {
				t.Errorf("personal command changed: %q, %v", got, err)
			}
			if _, err := os.Stat(installedExecutable(home)); err != nil {
				t.Errorf("personal app removed: %v", err)
			}
			if !tc.missingPlist {
				if got, err := os.ReadFile(systemAgentPath); err != nil || string(got) != renderSystemPlist() {
					t.Errorf("pre-removal changed the system plist: %v", err)
				}
			}
		})
	}
}

func TestWaitForServiceGoneFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exit      int
		wantError bool
	}{
		{"gone", 113, false}, {"logged out", 112, false},
		{"inspection failed", 1, true}, {"still loaded", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := launchctl
			launchctl = filepath.Join(t.TempDir(), "launchctl")
			t.Cleanup(func() { launchctl = previous })
			writeUninstallFixture(t, launchctl, fmt.Sprintf("#!/bin/sh\nexit %d\n", tc.exit), 0o700)
			err := waitForServiceGone("gui/501/com.quesma.shipper", 0)
			if (err != nil) != tc.wantError {
				t.Fatalf("wait = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestSystemRemovalChecksEachDistinctAccount(t *testing.T) {
	previousPath, previousLaunchctl := systemAgentPath, launchctl
	systemAgentPath = filepath.Join(t.TempDir(), "system-agent.plist")
	launchctl = filepath.Join(t.TempDir(), "launchctl")
	t.Cleanup(func() { systemAgentPath, launchctl = previousPath, previousLaunchctl })
	writeUninstallFixture(t, systemAgentPath, renderSystemPlist(), 0o644)
	marker := filepath.Join(t.TempDir(), "bootout")
	fake := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
"print gui/501/com.quesma.shipper") printf '\tpath = /Users/personal/Library/LaunchAgents/com.quesma.shipper.plist\n' ;;
"print gui/502/com.quesma.shipper")
  [ ! -e %q ] || exit 113
  printf '\tpath = %%s\n' %q ;;
"bootout gui/502/com.quesma.shipper") touch %q ;;
"print gui/503/com.quesma.shipper") exit 112 ;;
*) exit 99 ;;
esac
`, marker, systemAgentPath, marker)
	writeUninstallFixture(t, launchctl, fake, 0o700)
	if err := preUninstallSystem([]localUser{{uid: "501"}, {uid: "502"}, {uid: "502"}, {uid: "503"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("system service was not stopped after skipping personal service: %v", err)
	}
}

func TestLaunchdServicePathUsesOnlyUnambiguousServiceField(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"system", "service = {\n\tpath = /Library/LaunchAgents/com.quesma.shipper.plist\n}", "/Library/LaunchAgents/com.quesma.shipper.plist"},
		{"spaces", "\tpath = /Users/person one/Library/LaunchAgents/com.quesma.shipper.plist\n", "/Users/person one/Library/LaunchAgents/com.quesma.shipper.plist"},
		{"nested", "\tenvironment = {\n\t\tpath = /Library/LaunchAgents/com.quesma.shipper.plist\n\t}\n", ""},
		{"duplicate", "\tpath = /system.plist\n\tpath = /personal.plist\n", ""},
		{"relative", "\tpath = relative.plist\n", ""},
		{"missing", "\tprogram = /Applications/Quesma Shipper.app/Contents/MacOS/quesma-shipper\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchdServicePath(tc.output); got != tc.want {
				t.Fatalf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSystemRemovalRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root user")
	}
	if err := PreUninstallSystem(); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("PreUninstallSystem = %v", err)
	}
}

func writeUninstallFixture(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
