//go:build darwin

package macos

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemLaunchAgentUsesTheLoginIdentity(t *testing.T) {
	plist := renderSystemPlist()
	var v any
	if err := xml.Unmarshal([]byte(plist), &v); err != nil {
		t.Fatal(err)
	}
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
