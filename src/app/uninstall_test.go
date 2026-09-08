package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUninstallKeepsStateUnlessPurged(t *testing.T) {
	for _, purge := range []bool{false, true} {
		stateDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateDir, "enrollment.json"), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
		var steps []UninstallStep
		if err := uninstallState(stateDir, purge, func(step UninstallStep) { steps = append(steps, step) }); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(stateDir)
		if purge && !os.IsNotExist(err) {
			t.Errorf("purge left state behind: %v", err)
		}
		if !purge && err != nil {
			t.Errorf("ordinary uninstall removed state: %v", err)
		}
		if len(steps) != 1 || (purge && steps[0].Done == "") || (!purge && steps[0].Skip == "") {
			t.Errorf("purge=%v reported %#v", purge, steps)
		}
	}
}
