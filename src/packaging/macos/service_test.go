//go:build darwin

package macos

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{Executable: "/usr/local/bin/quesma-shipper", Args: []string{"run"},
		Home: "/Users/jane", StateDir: "/Users/jane/.local/state/trajectory-shipper",
		LogDir: "/Users/jane/.local/state/trajectory-shipper/logs"}
}

// A context that is already done makes exec.Cmd.Start fail before it spawns anything, so these
// never reach the developer's real supervisor.
func TestRestartHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestartService(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("restart with a canceled context = %v, want context canceled", err)
	}
}

func TestServiceStateHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ServiceState(ctx)
	if got.Loaded {
		t.Fatalf("state with a canceled context = %+v, want not loaded", got)
	}
}

func TestPlistIsWellFormedAndKeepsTheAgentAlive(t *testing.T) {
	got := renderPlist(testSpec())
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("invalid plist XML: %v", err)
	}
	for _, want := range []string{"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>",
		"<string>" + bundleIdentifier + "</string>", "<string>/usr/local/bin/quesma-shipper</string>",
		"<key>HOME</key>", "XDG_STATE_HOME", "StandardOutPath", "StandardErrorPath",
		"<key>AssociatedBundleIdentifiers</key>\n\t<array>\n\t\t<string>" + bundleIdentifier + "</string>"} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}
}

func TestPlistEscapesPaths(t *testing.T) {
	spec := testSpec()
	spec.Home = "/Users/jane & co"
	spec.Executable = "/opt/<weird>/quesma-shipper"
	got := renderPlist(spec)
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("an unescaped path broke the plist: %v\n%s", err, got)
	}
}

func TestLaunchAgentPathIsPerUser(t *testing.T) {
	got := launchdPath("/Users/jane")
	if !strings.Contains(got, "Library/LaunchAgents") || strings.Contains(got, "LaunchDaemons") {
		t.Errorf("unexpected launchd path: %q", got)
	}
}

func TestMixedInstallRemovesOnlyPersonalFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := installedApp(home)
	exe := writeTestApp(t, app, bundleIdentifier, executableName)
	personalPlist := launchdPath(home)
	personalSpec := testSpec()
	personalSpec.Executable = exe
	writeUninstallFixture(t, personalPlist, renderPlist(personalSpec), 0o600)
	stubSystemAgentPath(t)
	systemPlist := renderSystemPlist()
	writeUninstallFixture(t, systemAgentPath, systemPlist, 0o600)
	fakeLaunchctl(t)
	bootoutMarker := filepath.Join(t.TempDir(), "bootout")
	fake := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = print ]; then printf 'path = %%s\\n' %q; else touch %q; fi\n", systemAgentPath, bootoutMarker)
	writeUninstallFixture(t, launchctl, fake, 0o700)
	if err := uninstallPersonalService(exe, home); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveProgram(exe); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{personalPlist, app, bootoutMarker} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("personal cleanup left %s or stopped the system agent: %v", path, err)
		}
	}
	if got, err := os.ReadFile(systemAgentPath); err != nil || string(got) != systemPlist {
		t.Errorf("system LaunchAgent changed: %v", err)
	}
}

func TestPersonalUninstallStopsOnlyItsLoadedAgent(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "quesma-shipper")
	path := launchdPath(home)
	writeUninstallFixture(t, path, renderPlist(Spec{Executable: exe}), 0o600)
	fakeLaunchctl(t)
	marker := filepath.Join(t.TempDir(), "bootout")
	fake := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = print ]; then printf 'path = %%s\\n' %q; else touch %q; fi\n", path, marker)
	writeUninstallFixture(t, launchctl, fake, 0o700)
	if err := uninstallPersonalService(exe, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("personal agent was not stopped: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("personal plist remains: %v", err)
	}
}
