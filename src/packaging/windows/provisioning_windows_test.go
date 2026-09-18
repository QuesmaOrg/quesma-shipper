//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"testing"
)

// A file this test process wrote is owned by, or writable by, an ordinary account: never trusted.
func TestTrustedMachineFileRejectsWhatAnOrdinaryUserWrote(t *testing.T) {
	if currentSID(t) == sidLocalSystem {
		t.Skip("running as SYSTEM, whose files are trusted by definition")
	}
	path := filepath.Join(t.TempDir(), "provisioning.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TrustedMachineFile(path); err == nil {
		t.Fatal("TrustedMachineFile() = nil for a file an ordinary user can change")
	}
}
