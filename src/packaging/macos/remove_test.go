//go:build darwin

package macos

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestRemoveProgramRemovesAppAndItsCLILink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := filepath.Join(home, "Applications", "Shipper.app")
	executable := writeTestApp(t, app, common.Label, "shipper")
	link := filepath.Join(home, ".local", "bin", "quesma-shipper")
	legacyLink := filepath.Join(home, ".local", "bin", "shipper")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, legacyLink); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveProgram(executable)
	if err != nil {
		t.Fatal(err)
	}
	if removed != app {
		t.Fatalf("removed path = %q, want %q", removed, app)
	}
	for _, path := range []string{app, link, legacyLink} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
}

func TestRemoveProgramRefusesAnUnrelatedApp(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Other.app")
	executable := writeTestApp(t, app, "com.example.other", "shipper")
	if _, err := RemoveProgram(executable); err == nil {
		t.Fatal("uninstall accepted an unrelated app")
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("uninstall damaged the unrelated app: %v", err)
	}
}
