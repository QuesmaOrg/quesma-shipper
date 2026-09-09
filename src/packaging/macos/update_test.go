//go:build darwin

package macos

import (
	"os"
	"os/exec"
	"path/filepath"
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

// testAppPackage builds a product with the renamed component plus any extra component packages.
func testAppPackage(t *testing.T, version string, extra ...string) []byte {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "Applications", appName)
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
	if err := os.WriteFile(filepath.Join(app, "new"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	component := filepath.Join(work, componentPackage)
	if out, err := exec.Command("/usr/bin/pkgbuild", "--root", root, "--identifier", bundleIdentifier,
		"--version", "1", component).CombinedOutput(); err != nil {
		t.Fatalf("pkgbuild: %v: %s", err, out)
	}
	pkg := filepath.Join(work, "quesma-shipper.pkg")
	args := []string{"--package", component}
	for _, c := range extra {
		args = append(args, "--package", c)
	}
	if out, err := exec.Command("/usr/bin/productbuild", append(args, pkg)...).CombinedOutput(); err != nil {
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
