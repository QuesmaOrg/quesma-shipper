package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestJudgeTick(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}

	r.JudgeTick(errors.New("flush failed"), formats.Report{}, false, platform.Delta{})
	rec := readFailureRecord(r.eff.StateDir)
	if rec.ConsecutiveFailures != 1 || rec.Latest() == nil {
		t.Fatalf("first failure not recorded: %+v", rec)
	}
	if rec.Latest().Kind != formats.FailureTick {
		t.Errorf("a plain error was recorded as %q", rec.Latest().Kind)
	}
	if rec.Latest().At == "" {
		t.Error("the failure carries no timestamp")
	}

	r.JudgeTick(errors.New("still failing"), formats.Report{}, true, platform.Delta{})
	rec = readFailureRecord(r.eff.StateDir)
	if rec.ConsecutiveFailures != 2 || rec.Latest().Message != "still failing" || rec.Latest().Kind != formats.FailurePanic {
		t.Fatalf("second failure did not update the record: %+v", rec)
	}

	// Recovery resets the count and keeps the failure: "when did this install last fail" outlives
	// the fix.
	r.JudgeTick(nil, formats.Report{}, false, platform.Delta{})
	rec = readFailureRecord(r.eff.StateDir)
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("success left consecutive_failures at %d", rec.ConsecutiveFailures)
	}
	if rec.Latest() == nil || rec.Latest().Message != "still failing" {
		t.Errorf("recovery erased the last failure: %+v", rec)
	}
}

// The two classification corrections: lock contention is neither a success nor a failure, and a nil
// error that shipped nothing while everything attempted failed is a failure.
func TestJudgeTickClassifies(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}

	if tickErr := r.JudgeTick(
		fmt.Errorf("open store: %w", engine.ErrLocked), formats.Report{}, false, platform.Delta{}); tickErr != nil {
		t.Errorf("lock contention was judged a failure: %v", tickErr)
	}
	if rec := readFailureRecord(r.eff.StateDir); rec.ConsecutiveFailures != 0 || rec.Latest() != nil {
		t.Errorf("lock contention wrote a record: %+v", rec)
	}

	tickErr := r.JudgeTick(nil, formats.Report{
		Failed: 3,
		Sources: []formats.SourceOutcome{{Files: []formats.FileOutcome{
			{Decision: formats.DecisionFailed, Reason: "the control plane authorized nothing"},
		}}},
	}, false, platform.Delta{})
	if tickErr == nil {
		t.Fatal("an all-uploads-failed run did not classify as a failure")
	}
	// The count alone cannot tell a refused PUT from an unreachable control plane, so the reason
	// has to travel: without it every such event reads identically and the log is unactionable.
	if !strings.Contains(tickErr.Error(), "authorized nothing") {
		t.Errorf("the verdict does not say why nothing shipped: %v", tickErr)
	}
	if rec := readFailureRecord(r.eff.StateDir); rec.ConsecutiveFailures != 1 || rec.Latest() == nil {
		t.Errorf("an all-uploads-failed run did not record: %+v", rec)
	}
}

// A run that shipped something is not a failure, however much also failed beside it.
func TestJudgeTickAcceptsAPartialRun(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}

	if tickErr := r.JudgeTick(nil, formats.Report{Shipped: 1, Failed: 3}, false, platform.Delta{}); tickErr != nil {
		t.Errorf("a run that shipped one file was called a failure: %v", tickErr)
	}
}

func TestJudgeTickPlaceholdersTheUsername(t *testing.T) {
	dir := t.TempDir()
	name := engine.UsernameFromStateDir(dir)
	if name == "" {
		t.Skip("no resolvable username on this machine")
	}
	r := &Runtime{eff: &config.Effective{StateDir: dir}}
	r.JudgeTick(fmt.Errorf("open /Users/%s/transcript.jsonl: permission denied", name), formats.Report{}, false, platform.Delta{})
	rec := readFailureRecord(r.eff.StateDir)
	if rec.Latest() == nil {
		t.Fatal("no failure recorded")
	}
	if strings.Contains(rec.Latest().Message, "/"+name+"/") {
		t.Errorf("the persisted message still carries the username: %q", rec.Latest().Message)
	}
}

// The bound is the whole design: this rides a document that already ships every run, so it must
// not grow. Newest kept, oldest dropped, and consecutive heartbeats overlap so a version nobody
// read loses nothing.
func TestTheFailureLogIsBoundedAndKeepsTheNewest(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}

	for i := range formats.MaxRecentFailures + 15 {
		r.JudgeTick(fmt.Errorf("failure number %d", i), formats.Report{}, false, platform.Delta{})
	}

	rec := readFailureRecord(r.eff.StateDir)
	if len(rec.Recent) != formats.MaxRecentFailures {
		t.Fatalf("the log holds %d events, want the cap of %d", len(rec.Recent), formats.MaxRecentFailures)
	}
	// Oldest first, so the last element is the newest failure.
	if got := rec.Latest().Message; got != fmt.Sprintf("failure number %d", formats.MaxRecentFailures+14) {
		t.Errorf("the newest event is %q", got)
	}
	if got := rec.Recent[0].Message; got != fmt.Sprintf("failure number %d", 15) {
		t.Errorf("the oldest kept event is %q; the log should have dropped the first 15", got)
	}
	// The count is not the log's length: it counts runs since the last success, unbounded.
	if rec.ConsecutiveFailures != formats.MaxRecentFailures+15 {
		t.Errorf("consecutive_failures = %d, want every failed run counted", rec.ConsecutiveFailures)
	}
}

// Recovery clears the count but keeps the log: what happened is still worth reading after the fix.
func TestRecoveryKeepsTheLog(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}
	r.JudgeTick(errors.New("the sink refused"), formats.Report{}, false, platform.Delta{})
	r.JudgeTick(nil, formats.Report{Shipped: 1}, false, platform.Delta{})

	rec := readFailureRecord(r.eff.StateDir)
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d after a success", rec.ConsecutiveFailures)
	}
	if len(rec.Recent) != 1 || rec.Latest().Message != "the sink refused" {
		t.Errorf("recovery erased the log: %+v", rec.Recent)
	}
}

// A panic in any verb has to outlive the terminal it printed to. The state dir is resolved without
// the configuration on purpose, so this works on an install whose config is what broke.
func TestRecordPanicPersistsWithoutAResolvedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	stateDir, err := StateDirWithoutConfig()
	if err != nil {
		t.Skipf("no resolvable state directory here: %v", err)
	}

	RecordPanic("enroll", "runtime error: index out of range [3] with length 0")

	rec := readFailureRecord(stateDir)
	if rec.Latest() == nil {
		t.Fatalf("the panic was not persisted under %s", stateDir)
	}
	if rec.Latest().Kind != formats.FailurePanic {
		t.Errorf("a panic was recorded as %q", rec.Latest().Kind)
	}
	if !strings.Contains(rec.Latest().Message, "enroll") {
		t.Errorf("the record does not name the verb that crashed: %q", rec.Latest().Message)
	}
	if !strings.Contains(rec.Latest().Message, "index out of range") {
		t.Errorf("the record does not say what happened: %q", rec.Latest().Message)
	}
	// The count answers "how many COLLECTION runs failed in a row". A one-shot verb crashing is
	// not one of those, and moving the count would report a broken daemon on a healthy install.
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("a verb panic moved the collection failure count to %d", rec.ConsecutiveFailures)
	}
}

// The heartbeat's failure half is read off disk, not built from the run assembling it: a run that
// got far enough to upload is by definition not the one that failed.
func TestFailureRecordSurvivesIntoTheHeartbeat(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}}
	r.JudgeTick(errors.New("the sink refused every object"), formats.Report{}, false, platform.Delta{})

	r2 := &Runtime{eff: &config.Effective{StateDir: dir}}
	rec := r2.failureRecord()
	if rec.Latest() == nil || rec.Latest().Message != "the sink refused every object" {
		t.Fatalf("a later run did not pick up the persisted failure: %+v", rec)
	}
	if rec.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", rec.ConsecutiveFailures)
	}
}

// Store corruption is the one condition that can lose a file permanently, and it does not fail the
// run that finds it. Recorded so it is visible, not counted so it does not read as a broken daemon.
func TestStoreCorruptionIsRecordedButNotCounted(t *testing.T) {
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}}

	if tickErr := r.JudgeTick(nil, formats.Report{StoreCorrupt: true, Shipped: 3}, false, platform.Delta{}); tickErr != nil {
		t.Fatalf("a corrupt store must not fail the run: %v", tickErr)
	}
	rec := readFailureRecord(r.eff.StateDir)
	if rec.Latest() == nil || rec.Latest().Kind != formats.FailureStoreCorrupt {
		t.Fatalf("the discard was not recorded: %+v", rec.Recent)
	}
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("a discarded store moved the failed-run count to %d", rec.ConsecutiveFailures)
	}
}

// The point of the field: a reader has to be able to tell twenty runs that each failed once from
// one run that failed twenty times, which a flat list of messages cannot express.
func TestEventsAreAttributedToTheRunThatRecordedThem(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: id}
		r.JudgeTick(errors.New("boom"), formats.Report{}, false, platform.Delta{})
	}
	rec := readFailureRecord(dir)
	var got []string
	for _, e := range rec.Recent {
		got = append(got, e.Kind+"/"+e.RunID)
	}
	want := []string{"tick_failed/aaaaaaaaaaaaaaaa", "tick_failed/bbbbbbbbbbbbbbbb"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tick events not attributed:\n got %v\nwant %v", got, want)
	}
}

// The startup path has no Runtime to carry the id -- that is the failure it reports -- so the
// caller hands it over instead, and this is the only thing proving it does.
func TestAStartupFailureIsAttributedToItsRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := StateDirWithoutConfig()
	if err != nil {
		t.Skipf("no resolvable state directory here: %v", err)
	}

	RecordStartupFailure("sync", "cccccccccccccccc", errors.New("unparseable"))

	rec := readFailureRecord(stateDir)
	latest := rec.Latest()
	if latest == nil {
		t.Fatalf("the startup failure was not persisted under %s", stateDir)
	}
	if latest.RunID != "cccccccccccccccc" {
		t.Errorf("startup failure carries run id %q, want the one the caller passed", latest.RunID)
	}
}

// Self-update is the remediation channel: an install that cannot replace itself cannot be fixed
// remotely, so the one failure that must never be stderr-only is this one. Uncounted, because
// collection around it succeeded.
func TestAnUpdateFailureIsRecordedButNotCounted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := StateDirWithoutConfig()
	if err != nil {
		t.Skipf("no resolvable state directory here: %v", err)
	}

	RecordUpdateFailure("self-update from 1.2.3 did not happen: tuf: no such target")

	rec := readFailureRecord(stateDir)
	latest := rec.Latest()
	if latest == nil {
		t.Fatalf("the update failure was not persisted under %s", stateDir)
	}
	if latest.Kind != formats.FailureUpdate {
		t.Errorf("recorded as %q, want %q", latest.Kind, formats.FailureUpdate)
	}
	if !strings.Contains(latest.Message, "tuf: no such target") {
		t.Errorf("the record does not say why the update failed: %q", latest.Message)
	}
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("a failed self-update moved the collection failure count to %d", rec.ConsecutiveFailures)
	}
}

// The facts ride every outcome, a clean one included: memory pressure and a pathological redaction
// degrade an install without any step erroring, so the healthy run's cost is the baseline that makes
// the next one readable. Figures rather than events, because they would recur every tick and evict
// the log.
func TestTheFactsRideACleanRunToo(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir, MaxFilesPerRun: 512}}
	mem := platform.Delta{
		Before: platform.Sample{HeapInuse: 1 << 20, NumGC: 5},
		After:  platform.Sample{HeapInuse: 9 << 20, Sys: 40 << 20, NumGC: 12},
	}

	r.JudgeTick(nil, formats.Report{SlowestScrubNanos: 2_500_000, SlowestScrubBytes: 4096}, false, mem)

	f := readFailureRecord(dir).Facts
	if f == nil {
		t.Fatal("a clean run recorded no facts, so there is no baseline to compare against")
	}
	if f.HeapInuseBytes != 9<<20 || f.SysBytes != 40<<20 {
		t.Errorf("memory not carried: heap %d sys %d", f.HeapInuseBytes, f.SysBytes)
	}
	// The delta, not the absolute: GC cycles rise sharply as the heap nears the limit.
	if f.GCCycles != 7 {
		t.Errorf("gc cycles = %d, want the delta 7", f.GCCycles)
	}
	if f.SlowestScrubNanos != 2_500_000 || f.SlowestScrubBytes != 4096 {
		t.Errorf("the slowest redaction did not travel: %d ns over %d bytes",
			f.SlowestScrubNanos, f.SlowestScrubBytes)
	}
	if f.MaxFilesPerRun != 512 || f.GOMAXPROCS == 0 || f.MaxInFlightBytes == 0 {
		t.Errorf("the concurrency configuration is incomplete: %+v", f)
	}
}
