//go:build linux

package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepointUnitRewritesOnlyTheExecStartPath(t *testing.T) {
	spec := testSpec()
	spec.Executable = "/home/jane/.local/bin/shipper"
	path := filepath.Join(t.TempDir(), unitName)
	if err := os.WriteFile(path, []byte(renderUnit(spec)), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := repointUnit(path, spec.Executable, "/home/jane/.local/bin/quesma-shipper")
	if err != nil || !changed {
		t.Fatalf("repointUnit() = %v, %v", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spec.Executable = "/home/jane/.local/bin/quesma-shipper"
	if string(got) != renderUnit(spec) {
		t.Fatalf("repointed unit differs from a fresh render:\n%s", got)
	}
	if !strings.Contains(string(got), "ExecStart=/home/jane/.local/bin/quesma-shipper run") {
		t.Fatalf("ExecStart was not repointed:\n%s", got)
	}

	if changed, err := repointUnit(path, "/home/jane/.local/bin/shipper", "/x"); err != nil || changed {
		t.Fatalf("an already repointed unit was rewritten: %v, %v", changed, err)
	}
	if changed, err := repointUnit(filepath.Join(t.TempDir(), "missing"), "/a", "/b"); err != nil || changed {
		t.Fatalf("a missing unit was not a no-op: %v, %v", changed, err)
	}
}
