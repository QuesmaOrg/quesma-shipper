//go:build darwin

package macos

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestShipperAppForExecutableValidatesIdentity(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Shipper.app")
	executable := writeTestApp(t, app, common.Label, "shipper")
	if got, ok := shipperAppForExecutable(executable); !ok || got != app {
		t.Fatalf("shipperAppForExecutable() = %q, %v", got, ok)
	}
	if _, ok := shipperAppForExecutable("/Users/me/.local/bin/shipper"); ok {
		t.Fatal("a raw binary was treated as an app bundle")
	}
	other := filepath.Join(t.TempDir(), "Other.app")
	if _, ok := shipperAppForExecutable(writeTestApp(t, other, "com.example.other", "shipper")); ok {
		t.Fatal("an unrelated app was treated as Shipper")
	}
	wrongName := filepath.Join(t.TempDir(), "Shipper.app")
	if _, ok := shipperAppForExecutable(writeTestApp(t, wrongName, common.Label, "helper")); ok {
		t.Fatal("an unrelated executable was treated as Shipper")
	}
}

func TestApplyAppPackageReplacesTheWholeBundle(t *testing.T) {
	parent := t.TempDir()
	app := filepath.Join(parent, "Shipper.app")
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

func testAppPackage(t *testing.T, version string) []byte {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "Applications", "Shipper.app")
	executable := writeTestApp(t, app, common.Label, "shipper")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.quesma.trajectory-shipper</string>
<key>ShipperReleaseVersion</key><string>` + version + `</string>
</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "new"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	component := filepath.Join(work, "Shipper-component.pkg")
	if out, err := exec.Command("/usr/bin/pkgbuild", "--root", root, "--identifier", common.Label,
		"--version", "1", component).CombinedOutput(); err != nil {
		t.Fatalf("pkgbuild: %v: %s", err, out)
	}
	pkg := filepath.Join(work, "Shipper.pkg")
	if out, err := exec.Command("/usr/bin/productbuild", "--package", component, pkg).CombinedOutput(); err != nil {
		t.Fatalf("productbuild: %v: %s", err, out)
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
