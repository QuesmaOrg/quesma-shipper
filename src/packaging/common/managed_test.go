package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedInstallReadsTheScopeMarkerBesideTheProgram(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quesma-shipper.exe")
	if ManagedInstall(exe) {
		t.Fatal("no marker was read as a managed install")
	}
	marker := filepath.Join(dir, scopeMarker)
	if err := os.WriteFile(marker, []byte("user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ManagedInstall(exe) {
		t.Fatal("a marker that does not say machine was read as a managed install")
	}
	if err := os.WriteFile(marker, []byte("machine\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ManagedInstall(exe) {
		t.Fatal("the machine marker was not recognized")
	}
}
