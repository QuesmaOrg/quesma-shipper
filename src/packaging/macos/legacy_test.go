//go:build darwin

package macos

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// formerUpdaterAccepts is the check every install older than the rename still runs on a downloaded
// package, copied from that code on purpose: it must not follow later refactors.
func formerUpdaterAccepts(expanded, version string) error {
	app := filepath.Join(expanded, "Shipper-component.pkg", "Payload", "Applications", "Shipper.app")
	info, err := os.Lstat(app)
	if err != nil {
		return fmt.Errorf("update does not contain Shipper.app: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("update Shipper.app is not a directory")
	}
	plist := filepath.Join(app, "Contents", "Info.plist")
	checks := map[string]string{
		"CFBundleIdentifier":    "com.quesma.trajectory-shipper",
		"ShipperReleaseVersion": version,
	}
	for key, want := range checks {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("reading %s from update: %w: %s", key, err, strings.TrimSpace(string(out)))
		}
		if got := strings.TrimSpace(string(out)); got != want {
			return fmt.Errorf("update %s is %q, want %q", key, got, want)
		}
	}
	executable := filepath.Join(app, "Contents", "MacOS", "shipper")
	if info, err := os.Stat(executable); err != nil || info.Mode()&0o111 == 0 {
		return errors.New("update has no executable Contents/MacOS/shipper")
	}
	return nil
}

func expandPackage(t *testing.T, raw []byte) string {
	t.Helper()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "update.pkg")
	if err := os.WriteFile(pkg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	expanded := filepath.Join(dir, "expanded")
	if out, err := exec.Command("/usr/sbin/pkgutil", "--expand-full", pkg, expanded).CombinedOutput(); err != nil {
		t.Fatalf("pkgutil --expand-full: %v: %s", err, out)
	}
	return expanded
}

func testLegacyComponent(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "Applications", legacyAppName)
	writeTestApp(t, app, legacyBundleIdentifier, legacyExecutableName)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>` + legacyBundleIdentifier + `</string>
<key>ShipperReleaseVersion</key><string>` + version + `</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	component := filepath.Join(t.TempDir(), "Shipper-component.pkg")
	if out, err := exec.Command("/usr/bin/pkgbuild", "--root", root, "--identifier", legacyBundleIdentifier,
		"--version", "1", component).CombinedOutput(); err != nil {
		t.Fatalf("pkgbuild: %v: %s", err, out)
	}
	return component
}

func TestBridgedPackageServesBothUpdaters(t *testing.T) {
	raw := testAppPackage(t, "1.2.3", testLegacyComponent(t, "1.2.3"))
	if err := formerUpdaterAccepts(expandPackage(t, raw), "1.2.3"); err != nil {
		t.Fatalf("the former updater rejects the bridged package: %v", err)
	}

	parent := t.TempDir()
	app := filepath.Join(parent, appName)
	if err := installAppPackage(raw, app, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := appForExecutable(filepath.Join(app, "Contents", "MacOS", executableName)); !ok {
		t.Fatalf("%s was not laid out as the renamed app", app)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging left something beside the app: %v, %v", entries, err)
	}
}

// The real artifact, when CI points at it: both layouts inside, and Installer selecting only the
// renamed one.
func TestBuiltPackageSatisfiesTheFormerUpdater(t *testing.T) {
	pkg, version := os.Getenv("QUESMA_SHIPPER_PKG"), os.Getenv("QUESMA_SHIPPER_RELEASE_VERSION")
	if pkg == "" || version == "" {
		t.Skip("set QUESMA_SHIPPER_PKG and QUESMA_SHIPPER_RELEASE_VERSION to check a built package")
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	expanded := expandPackage(t, raw)
	if err := formerUpdaterAccepts(expanded, version); err != nil {
		t.Fatalf("the former updater rejects the built package: %v", err)
	}
	renamed := filepath.Join(expanded, componentPackage, "Payload", "Applications", appName)
	if err := validateAppBundle(renamed, version); err != nil {
		t.Fatalf("the current updater rejects the built package: %v", err)
	}

	selected := installerChoices(t, pkg)
	if !selected[bundleIdentifier] || selected[legacyBundleIdentifier] {
		t.Fatalf("Installer choice selection = %v; the bridge must never be laid out", selected)
	}
}

// installerChoices reads Installer's own view of what the package would lay out.
func installerChoices(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	show := exec.Command("/usr/sbin/installer", "-showChoicesXML", "-pkg", pkg, "-target", "CurrentUserHomeDirectory")
	convert := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	xml, err := show.Output()
	if err != nil {
		t.Fatalf("installer -showChoicesXML: %v", err)
	}
	convert.Stdin = strings.NewReader(string(xml))
	out, err := convert.Output()
	if err != nil {
		t.Fatalf("plutil -convert json: %v", err)
	}
	var choices []installerChoice
	if err := json.Unmarshal(out, &choices); err != nil {
		t.Fatalf("parsing choices: %v\n%s", err, out)
	}
	selected := map[string]bool{}
	var walk func([]installerChoice)
	walk = func(cs []installerChoice) {
		for _, c := range cs {
			// Only leaves carry a package; a group reports -1 for a mixed selection.
			if len(c.Children) == 0 {
				selected[c.Identifier] = c.Selected == 1
			}
			walk(c.Children)
		}
	}
	walk(choices)
	return selected
}

type installerChoice struct {
	Identifier string            `json:"choiceIdentifier"`
	Selected   int               `json:"choiceIsSelected"`
	Children   []installerChoice `json:"childItems"`
}

func TestLegacyAppForExecutableAcceptsOnlyTheFormerIdentity(t *testing.T) {
	app := filepath.Join(t.TempDir(), legacyAppName)
	exe := writeTestApp(t, app, legacyBundleIdentifier, legacyExecutableName)
	if got, ok := legacyAppForExecutable(exe); !ok || got != app {
		t.Fatalf("legacyAppForExecutable() = %q, %v", got, ok)
	}
	renamed := filepath.Join(t.TempDir(), appName)
	if _, ok := legacyAppForExecutable(writeTestApp(t, renamed, bundleIdentifier, executableName)); ok {
		t.Fatal("the renamed app was treated as the former one")
	}
	other := filepath.Join(t.TempDir(), legacyAppName)
	if _, ok := legacyAppForExecutable(writeTestApp(t, other, "com.example.other", legacyExecutableName)); ok {
		t.Fatal("an unrelated app with the former name was treated as the former install")
	}
}

func TestRetireLegacyInstallRemovesWhatTheFormerPackageLeft(t *testing.T) {
	booted := false
	stubBootout(t, func(bool) { booted = true })

	home := t.TempDir()
	app := legacyAppPath(home)
	writeTestApp(t, app, legacyBundleIdentifier, legacyExecutableName)
	link := filepath.Join(home, ".local", "bin", legacyExecutableName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../Applications/Shipper.app/Contents/MacOS/shipper", link); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", legacyBundleIdentifier+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	retireLegacyInstall(home, app, false)
	for _, path := range []string{app, link, plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
	if !booted {
		t.Error("the former LaunchAgent was not booted out")
	}
}

func TestRetireLegacyInstallLeavesUnrelatedThingsAlone(t *testing.T) {
	stubBootout(t, func(bool) { t.Error("bootout ran with no former LaunchAgent present") })

	home := t.TempDir()
	app := legacyAppPath(home)
	writeTestApp(t, app, "com.example.other", legacyExecutableName)
	link := filepath.Join(home, ".local", "bin", legacyExecutableName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/true", link); err != nil {
		t.Fatal(err)
	}

	retireLegacyInstall(home, app, false)
	for _, path := range []string{app, link} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}

func stubBootout(t *testing.T, stub func(bool)) {
	t.Helper()
	real := bootoutLegacy
	bootoutLegacy = stub
	t.Cleanup(func() { bootoutLegacy = real })
}
