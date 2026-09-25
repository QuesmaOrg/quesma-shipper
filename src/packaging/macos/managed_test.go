//go:build darwin

package macos

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemLaunchAgentContainsExpectedSettings(t *testing.T) {
	plist := renderSystemPlist()
	for _, want := range []string{installedExecutable("/"), "RunAtLoad", "KeepAlive", "Aqua"} {
		if !strings.Contains(plist, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"HOME", "XDG_STATE_HOME", "UserName", "/var/root", "LaunchDaemons"} {
		if strings.Contains(plist, bad) {
			t.Errorf("system agent pins a user or state directory: %s", bad)
		}
	}
}

func TestSystemProgramRemovalRefusesBeforeTouchingFiles(t *testing.T) {
	if _, err := RemoveProgram(installedExecutable("/")); !errors.Is(err, errSystemManaged) {
		t.Fatalf("system bundle removal: %v", err)
	}
}

func TestPersonalUninstallRemainsAvailableWhenSystemAgentExists(t *testing.T) {
	previous := systemAgentPath
	systemAgentPath = filepath.Join(t.TempDir(), "system-agent.plist")
	t.Cleanup(func() { systemAgentPath = previous })
	if err := os.WriteFile(systemAgentPath, []byte("system agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !SystemManaged() {
		t.Fatal("test did not detect the system agent")
	}
	if err := ValidateUserUninstall(); err != nil {
		t.Fatalf("personal executable could not uninstall: %v", err)
	}
	if err := UninstallService(); !errors.Is(err, errSystemManaged) {
		t.Fatalf("direct service removal could stop the system agent: %v", err)
	}
}

func TestMixedInstallCheckIncludesDormantUserBundles(t *testing.T) {
	home := t.TempDir()
	if err := checkNoUserInstallation(home); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installedApp(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkNoUserInstallation(home); err == nil {
		t.Fatal("an unregistered personal app must prevent system installation")
	}
}

func TestParseLocalUsersDeduplicatesConsoleAccount(t *testing.T) {
	users, err := parseLocalUsers([]byte("501\t/Users/person one\n501\t/Users/person one\n501\t/Users/alternate\n502\t/Users/person two\n"))
	if err != nil || len(users) != 3 || users[0].home != "/Users/person one" {
		t.Fatalf("local users = %+v, %v", users, err)
	}
	if _, err := parseLocalUsers([]byte("503\trelative/home\n")); err == nil {
		t.Fatal("accepted a relative user home")
	}
}

func TestInstallCommandLinkPreservesAnotherInstallation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bin", executableName)
	exe := filepath.Join(root, executableName)
	if err := os.WriteFile(exe, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := installCLILink(path, exe); err != nil {
		t.Fatal(err)
	}
	if err := installCLILink(path, exe); err != nil {
		t.Fatal(err)
	}
	if err := installCLILink(path, filepath.Join(root, "other")); err == nil {
		t.Fatal("a competing link was accepted")
	}
	if target, err := os.Readlink(path); err != nil || target != exe {
		t.Fatalf("existing command was changed: %q, %v", target, err)
	}
}

func TestCoreFoundationPreferenceBindings(t *testing.T) {
	api, err := loadPreferences()
	if err != nil {
		t.Fatal(err)
	}
	domain := api.newString(0, "com.quesma.shipper.test.unmanaged", cfUTF8)
	defer api.release(domain)
	api.sync(domain)
	if value, err := api.read(domain, "NoSuchManagedKey"); err != nil || value != "" {
		t.Fatalf("unmanaged preference was accepted: error %v", err)
	}
}
