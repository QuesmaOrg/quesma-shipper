package windows

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestSupervisorRotatesLogsBeforeOpeningChildHandles(t *testing.T) {
	dir := t.TempDir()
	const oversized = 16 << 20
	for _, name := range []string{"agent.out.log", "agent.err.log"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		err = f.Truncate(oversized)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runChild(filepath.Join(t.TempDir(), "absent.exe"), dir); err == nil {
		t.Fatal("expected the missing child executable to fail after opening logs")
	}
	for _, name := range []string{"agent.out.log", "agent.err.log"} {
		previous, err := os.Stat(filepath.Join(dir, name+platform.PreviousLogSuffix))
		if err != nil {
			t.Fatalf("oversized %s was not rotated before opening its Windows handle: %v", name, err)
		}
		current, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if previous.Size() != oversized || current.Size() != 0 {
			t.Fatalf("%s rotation: previous bytes %d, new bytes %d", name, previous.Size(), current.Size())
		}
	}
}
