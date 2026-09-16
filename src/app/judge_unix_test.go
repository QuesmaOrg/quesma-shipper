//go:build unix

package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// A disk that cannot be written must not also erase the judgement: the record stays authoritative
// in memory, so the next heartbeat this process ships still carries the failure.
func TestJudgeKeepsTheRecordWhenTheDiskWillNot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	r.JudgeTick(errors.New("state: no space left on device"), formats.Report{}, false, platform.Delta{})

	rec := r.failureRecord()
	if rec.Latest() == nil || !strings.Contains(rec.Latest().Message, "no space left") {
		t.Fatalf("the unwritable failure did not survive in memory: %+v", rec)
	}
	if rec.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", rec.ConsecutiveFailures)
	}
	if disk := readFailureRecord(dir); disk.Latest() != nil {
		t.Fatalf("the read-only state dir somehow took a write: %+v", disk)
	}
}
