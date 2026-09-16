//go:build darwin

package macos

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppForExecutableValidatesIdentity(t *testing.T) {
	app := filepath.Join(t.TempDir(), appName)
	executable := writeTestApp(t, app, bundleIdentifier, executableName)
	if got, ok := appForExecutable(executable); !ok || got != app {
		t.Fatalf("appForExecutable() = %q, %v", got, ok)
	}
	if _, ok := appForExecutable("/Users/me/.local/bin/quesma-shipper"); ok {
		t.Fatal("a raw binary was treated as an app bundle")
	}
	other := filepath.Join(t.TempDir(), "Other.app")
	if _, ok := appForExecutable(writeTestApp(t, other, "com.example.other", executableName)); ok {
		t.Fatal("an unrelated app was treated as Quesma Shipper")
	}
	wrongName := filepath.Join(t.TempDir(), appName)
	if _, ok := appForExecutable(writeTestApp(t, wrongName, bundleIdentifier, "helper")); ok {
		t.Fatal("an unrelated executable was treated as Quesma Shipper")
	}
}

func TestApplyAppPackageReplacesTheWholeBundle(t *testing.T) {
	parent := t.TempDir()
	app := filepath.Join(parent, appName)
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	const version = "0.0.1-123.abcdef123456"
	if err := applyAppPackage(testAppPackage(t, version), app, version); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, "old")); !os.IsNotExist(err) {
		t.Fatalf("old bundle survived replacement: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(app, "new")); err != nil || string(got) != "new" {
		t.Fatalf("new bundle not installed: %q, %v", got, err)
	}
}

// The real artifact, when CI points at it: the updater accepts the bundle the package carries, and
// Installer would lay that component out.
func TestBuiltPackageSatisfiesTheUpdater(t *testing.T) {
	pkg, version := os.Getenv("QUESMA_SHIPPER_PKG"), os.Getenv("QUESMA_SHIPPER_RELEASE_VERSION")
	if pkg == "" || version == "" {
		t.Skip("set QUESMA_SHIPPER_PKG and QUESMA_SHIPPER_RELEASE_VERSION to check a built package")
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expandAppPackage(raw, t.TempDir(), version); err != nil {
		t.Fatalf("the updater rejects the built package: %v", err)
	}
	if selected := installerChoices(t, pkg); !selected[bundleIdentifier] {
		t.Fatalf("Installer choice selection = %v; %s must be laid out", selected, bundleIdentifier)
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

// testAppPackage builds a product archive holding the component the updater expects.
func testAppPackage(t *testing.T, version string) []byte {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "Applications", appName)
	writeTestBundle(t, app, version)
	if err := os.WriteFile(filepath.Join(app, "new"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	component := filepath.Join(work, componentPackage)
	if res, err := exec.Command("/usr/bin/pkgbuild", "--root", root, "--identifier", bundleIdentifier,
		"--version", "1", component).CombinedOutput(); err != nil {
		t.Fatalf("pkgbuild: %v: %s", err, res)
	}
	pkg := filepath.Join(work, "quesma-shipper.pkg")
	if out, err := exec.Command("/usr/bin/productbuild", "--package", component, pkg).CombinedOutput(); err != nil {
		t.Fatalf("productbuild: %v: %s", err, out)
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// writeTestBundle is writeTestApp plus the release-version key the updater validates.
func writeTestBundle(t *testing.T, app, version string) string {
	t.Helper()
	executable := writeTestApp(t, app, bundleIdentifier, executableName)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>` + bundleIdentifier + `</string>
<key>` + releaseVersionField + `</key><string>` + version + `</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func writeTestApp(t *testing.T, app, bundleID, executableName string) string {
	t.Helper()
	executable := filepath.Join(app, "Contents", "MacOS", executableName)
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + bundleID + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}
