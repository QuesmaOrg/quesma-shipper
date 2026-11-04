//go:build darwin

package macos

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMDMUninstallDeletesOnlyValidatedSystemFiles(t *testing.T) {
	for _, scenario := range []string{
		"normal", "already absent", "foreign CLI link", "regular CLI file", "preuninstall fails",
		"not root", "symlink app", "symlink plist", "symlink executable", "untrusted executable",
		"unrecognized app", "unrecognized plist", "plist without app",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			app := installedApp(root)
			exe := writeTestApp(t, app, bundleIdentifier, executableName)
			plist := filepath.Join(root, "Library/LaunchAgents/com.quesma.shipper.plist")
			link := filepath.Join(root, "usr/local/bin/quesma-shipper")
			marker := filepath.Join(root, "preuninstall")
			writeUninstallFixture(t, exe, "#!/bin/sh\n[ \"$1\" = preuninstall-system ] || exit 99\ntouch \"$TEST_MARKER\"\nexit \"$TEST_PREUNINSTALL_EXIT\"\n", 0o755)
			writeUninstallFixture(t, plist, renderPlist(Spec{Executable: exe}), 0o644)
			if err := installCLILink(link, exe); err != nil {
				t.Fatal(err)
			}
			personal := filepath.Join(root, "personal-enrollment")
			writeUninstallFixture(t, personal, "synthetic enrollment and upload history", 0o600)
			t.Setenv("TEST_MARKER", marker)
			t.Setenv("TEST_PREUNINSTALL_EXIT", "0")
			t.Setenv("TEST_UID", "0")
			t.Setenv("TEST_OWNER", "0")
			bin := filepath.Join(root, "bin")
			writeUninstallFixture(t, filepath.Join(bin, "id"), "#!/bin/sh\necho \"$TEST_UID\"\n", 0o755)
			writeUninstallFixture(t, filepath.Join(bin, "stat"), "#!/bin/sh\necho \"$TEST_OWNER\"\n", 0o755)
			writeUninstallFixture(t, filepath.Join(bin, "pkgutil"), "#!/bin/sh\nexit 1\n", 0o755)
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			wantError := true
			switch scenario {
			case "normal":
				wantError = false
			case "already absent":
				wantError = false
				if err := os.RemoveAll(app); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(plist); err != nil {
					t.Fatal(err)
				}
			case "foreign CLI link", "regular CLI file":
				wantError = false
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if scenario == "foreign CLI link" {
					if err := os.Symlink(personal, link); err != nil {
						t.Fatal(err)
					}
				} else {
					writeUninstallFixture(t, link, "unrelated command", 0o755)
				}
			case "preuninstall fails":
				t.Setenv("TEST_PREUNINSTALL_EXIT", "1")
			case "not root":
				t.Setenv("TEST_UID", "501")
			case "untrusted executable":
				t.Setenv("TEST_OWNER", "501")
			case "symlink app", "symlink plist", "symlink executable":
				path := map[string]string{"symlink app": app, "symlink plist": plist, "symlink executable": exe}[scenario]
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "unrecognized app":
				writeTestApp(t, app, "com.example.other", executableName)
			case "unrecognized plist":
				writeUninstallFixture(t, plist, renderPlist(Spec{Executable: "/other/program"}), 0o644)
			case "plist without app":
				if err := os.RemoveAll(app); err != nil {
					t.Fatal(err)
				}
			}

			// Relocate only the script's fixed system paths and root/account tools into the fixture.
			script, err := os.ReadFile("mdm/uninstall.sh")
			if err != nil {
				t.Fatal(err)
			}
			text := strings.NewReplacer(
				"/Applications/Quesma Shipper.app", app,
				"/Library/LaunchAgents", filepath.Dir(plist),
				"/usr/local/bin/quesma-shipper", link,
				"/usr/bin/stat", filepath.Join(bin, "stat"),
				"/usr/sbin/pkgutil", filepath.Join(bin, "pkgutil"),
			).Replace(string(script))
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Stdin = strings.NewReader(text)
			out, err := cmd.CombinedOutput()
			if (err != nil) != wantError {
				t.Fatalf("uninstall = %v, want error %v: %s", err, wantError, out)
			}
			for _, path := range []string{app, plist} {
				_, err := os.Lstat(path)
				wantPresent := wantError && !(scenario == "plist without app" && path == app)
				if (err == nil) != wantPresent {
					t.Errorf("path %s: %v, want present %v", path, err, wantPresent)
				}
			}
			if scenario == "normal" {
				if _, err := os.Stat(marker); err != nil {
					t.Errorf("preuninstall not called: %v", err)
				}
				if _, err := os.Lstat(link); !os.IsNotExist(err) {
					t.Errorf("system command remains: %v", err)
				}
			}
			if scenario == "foreign CLI link" || scenario == "regular CLI file" {
				if _, err := os.Stat(link); err != nil {
					t.Errorf("unrelated command removed: %v", err)
				}
			}
			if wantError && scenario != "preuninstall fails" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("untrusted installation executed: %v", err)
				}
			}
			if got, err := os.ReadFile(personal); err != nil || string(got) != "synthetic enrollment and upload history" {
				t.Errorf("personal state changed: %v", err)
			}
		})
	}
}
