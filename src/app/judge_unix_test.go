//go:build unix

package app

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// Telemetry added on main must see a failed tick even when its failure record could not reach disk.
func TestTelemetryKeepsUnwrittenFailureThroughRecovery(t *testing.T) {
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
	for _, recovered := range []bool{false, true} {
		wantCount := 1
		if recovered {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			r.JudgeTick(nil, formats.Report{Shipped: 1}, false, platform.Delta{})
			wantCount = 0
		}
		_, body, err := r.installHealth(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var event telemetryEvent
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		if event.Consecutive != wantCount || len(event.Faults) != 1 || !strings.Contains(event.Faults[0].Message, "no space left") {
			t.Fatalf("recovered=%v: telemetry lost the failure or its recovery: %+v", recovered, event)
		}
	}
	if disk := readFailureRecord(dir); disk.ConsecutiveFailures != 0 || disk.Latest() == nil {
		t.Fatalf("recovery did not persist the retained failure: %+v", disk)
	}
}
