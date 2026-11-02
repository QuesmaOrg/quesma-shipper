package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func TestJudgeTick(t *testing.T) {
	// With a run id, as the run loop always has one: a panicked tick counts only because it carries
	// one, and a fixture without it would assert the one-shot verb's rule against the tick's path.
	r := &Runtime{eff: &config.Effective{StateDir: t.TempDir()}, runID: "0123456789abcdef"}

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

// The re-enrollment case end to end: the discard that recovers the install must clear the streak

// those refusals built up, without the discard itself becoming the streak's newest reason.

func TestARecoveringRunClearsTheStreakItInherited(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "aaaaaaaaaaaaaaaa"}
		r.JudgeTick(errors.New("state: document belongs to another install"), formats.Report{}, false, platform.Delta{})
	}
	if rec := readFailureRecord(dir); rec.ConsecutiveFailures != 3 {
		t.Fatalf("the refusals did not build a streak: %d", rec.ConsecutiveFailures)
	}

	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "bbbbbbbbbbbbbbbb"}
	if err := r.JudgeTick(nil, formats.Report{StoreCorrupt: true, Shipped: 7}, false, platform.Delta{}); err != nil {
		t.Fatalf("the recovering run failed: %v", err)
	}

	rec := readFailureRecord(dir)
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("the recovery left %d failures on the streak", rec.ConsecutiveFailures)
	}
	if rec.Latest() == nil || rec.Latest().Kind != formats.FailureStoreCorrupt {
		t.Fatalf("the discard was not recorded: %+v", rec.Recent)
	}
	if last := rec.LatestCounted(); last == nil || last.Kind != formats.FailureTick {
		t.Errorf("LatestCounted picked the uncounted discard: %+v", last)
	}
	if rows := failureRows(dir, time.Now()); rows != nil {
		t.Errorf("doctor still reports failing runs after the recovery: %+v", rows)
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

// The crash used to reach only the heartbeat; the persisted event is what survives the process.

func TestACrashIsPersistedLocally(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := StateDirWithoutConfig()
	if err != nil {
		t.Skipf("no resolvable state directory here: %v", err)
	}

	RecordCrash(&formats.LastCrash{RunID: "dddddddddddddddd", Phase: "tick 3", Consecutive: 2})

	rec := readFailureRecord(stateDir)
	latest := rec.Latest()
	if latest == nil || latest.Kind != formats.FailureCrash {
		t.Fatalf("the crash was not persisted: %+v", rec)
	}
	if latest.RunID != "dddddddddddddddd" || !strings.Contains(latest.Message, `"tick 3"`) {
		t.Errorf("the event does not attribute the dead run: %+v", latest)
	}
	// Crashes keep their own counter; the collection count must not move.
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("a crash moved consecutive_failures to %d", rec.ConsecutiveFailures)
	}

	// An undelivered crash is re-detected on every restart; only a NEW dead run may append.
	RecordCrash(&formats.LastCrash{RunID: "dddddddddddddddd", Phase: "tick 3", Consecutive: 3})
	if rec := readFailureRecord(stateDir); len(rec.Recent) != 1 {
		t.Fatalf("a restart duplicated the crash event: %+v", rec.Recent)
	}
	RecordCrash(&formats.LastCrash{RunID: "ffffffffffffffff", Phase: "init", Consecutive: 2})
	if rec := readFailureRecord(stateDir); len(rec.Recent) != 2 {
		t.Fatalf("a different dead run was not recorded: %+v", rec.Recent)
	}
}

// One standing event however long the stall lasts: a stuck tick re-reported twenty times would

// evict the rest of the log.

func TestAStalledTickIsRecordedOnce(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "eeeeeeeeeeeeeeee"}
	watchFires(r, 7, 2)

	rec := readFailureRecord(dir)
	if len(rec.Recent) != 1 || rec.Latest().Kind != formats.FailureStalled {
		t.Fatalf("want exactly one stalled event, got %+v", rec.Recent)
	}
	if !strings.Contains(rec.Latest().Message, "tick 7") || rec.Latest().RunID != "eeeeeeeeeeeeeeee" {
		t.Errorf("the event does not name the tick or its run: %+v", rec.Latest())
	}
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("a stall moved consecutive_failures to %d; the tick may yet complete", rec.ConsecutiveFailures)
	}
}

// A stall that recovered leaves its event as the newest one through every clean tick after it; a

// later stall must replace it, not be deduplicated away behind a stale timestamp.

func TestALaterStallReplacesTheStandingEvent(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "ffffffffffffffff"}

	watchFires(r, 5, 1)
	for i := 0; i < 3; i++ {
		r.JudgeTick(nil, formats.Report{Shipped: 1}, false, platform.Delta{})
	}
	watchFires(r, 900, 1)

	rec := readFailureRecord(dir)
	if len(rec.Recent) != 1 || !strings.Contains(rec.Latest().Message, "tick 900") {
		t.Fatalf("want one stalled event naming tick 900, got %+v", rec.Recent)
	}
}

// fires counts watchdog fires: without an upload port the warning line is the only thing a fire

// writes, so waiting for a count replaces a wall-clock guess.

type fires chan struct{}

func (c fires) Write(p []byte) (int, error) { c <- struct{}{}; return len(p), nil }

func watchFires(r *Runtime, n, want int) {
	ctx, cancel := context.WithCancel(context.Background())
	fired := make(fires, 16)
	done := make(chan struct{})
	go func() {
		r.WatchStalledTick(ctx, n, time.Millisecond, fired)
		close(done)
	}()
	for i := 0; i < want; i++ {
		<-fired
	}
	cancel()
	<-done
}

// The in-memory copy exists for the disk that cannot be written; once a write succeeds it must be

// dropped, or the daemon would clobber events other processes append between its ticks.

func TestJudgeMergesEventsFromOtherWriters(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}}
	r.JudgeTick(errors.New("boom"), formats.Report{}, false, platform.Delta{})

	other := readFailureRecord(dir)
	other.Append(formats.FailureEvent{At: "2026-01-01T00:00:00Z", Kind: formats.FailureUpdate, Message: "tuf: no such target"})
	if err := writeFailureRecord(dir, other); err != nil {
		t.Fatal(err)
	}

	r.JudgeTick(nil, formats.Report{Shipped: 1}, false, platform.Delta{})
	rec := readFailureRecord(dir)
	if len(rec.Recent) != 2 {
		t.Fatalf("the other writer's event was clobbered: %+v", rec.Recent)
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

// Offline is the machine's state, not the shipper's: one standing event, nothing counted, and a

// later offline tick replaces it rather than filling the log with the same fact.

func TestAnOfflineTickIsRecordedOnceAndNotCounted(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "0123456789abcdef"}
	offline := fmt.Errorf("%w: collection stopped after 0 of 2 files: dial tcp: lookup cp.example: no such host", engine.ErrOffline)

	if err := r.JudgeTick(offline, formats.Report{Failed: 1}, false, platform.Delta{}); err == nil {
		t.Fatal("an offline tick still failed the run; the verdict must say so")
	}
	r.JudgeTick(offline, formats.Report{Failed: 1}, false, platform.Delta{})
	rec := readFailureRecord(dir)
	if len(rec.Recent) != 1 || rec.Latest().Kind != formats.FailureOffline {
		t.Fatalf("want exactly one offline event, got %+v", rec.Recent)
	}
	if !strings.HasPrefix(rec.Latest().Message, "offline: ") || !strings.Contains(rec.Latest().Message, "no such host") {
		t.Errorf("the event does not name the cause: %q", rec.Latest().Message)
	}
	if rec.ConsecutiveFailures != 0 {
		t.Errorf("an offline tick moved consecutive_failures to %d", rec.ConsecutiveFailures)
	}

	// A PUT that failed after the control plane answered reached the network: a failure.
	r.JudgeTick(nil, formats.Report{Failed: 2, Sources: []formats.SourceOutcome{{Files: []formats.FileOutcome{
		{Decision: formats.DecisionFailed, Reason: "upload: PUT object \"0\": dial tcp: lookup bucket.example: no such host"},
	}}}}, false, platform.Delta{})
	rec = readFailureRecord(dir)
	if rec.ConsecutiveFailures != 1 || rec.Latest().Kind != formats.FailureTick || len(rec.Recent) != 2 {
		t.Fatalf("a run that reached the network was not counted as a failure: %+v", rec)
	}
}

// The drain on a host going away with no network is the same fact as an offline tick, and it must

// not try to ship a failure heartbeat it cannot send.

func TestAnOfflineDrainIsNotAShutdownFailure(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}}
	r.JudgeFinalSlice(fmt.Errorf("%w: collection stopped", engine.ErrOffline), formats.Report{Failed: 1}, platform.Delta{})
	rec := readFailureRecord(dir)
	if rec.Latest() == nil || rec.Latest().Kind != formats.FailureOffline || rec.ConsecutiveFailures != 0 {
		t.Fatalf("an offline drain was judged as %+v", rec)
	}
}

// A stall and an offline verdict can alternate for as long as a backlog lasts; between them they

// hold two entries, and the counted failure before them stays in the log.

func TestStandingEventsDoNotEvictCountedFailures(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "abababababababab"}
	r.JudgeTick(errors.New("scrub: pattern timed out"), formats.Report{}, false, platform.Delta{})
	offline := fmt.Errorf("%w: collection stopped", engine.ErrOffline)
	for i := 0; i < 10; i++ {
		watchFires(r, i+2, 1)
		r.JudgeTick(offline, formats.Report{Failed: 1}, false, platform.Delta{})
	}
	rec := readFailureRecord(dir)
	if len(rec.Recent) != 3 {
		t.Fatalf("want the failure plus one stall and one offline entry, got %d: %+v", len(rec.Recent), rec.Recent)
	}
	if rec.LatestCounted() == nil || rec.ConsecutiveFailures != 1 {
		t.Fatalf("the counted failure was evicted: %+v", rec)
	}
}

// A run that failed a PUT and then lost the network is a failed run: the earlier failure reached

// the network, and going offline afterwards must not hide it.

func TestAFailureBeforeGoingOfflineStillCounts(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "0123456789abcdef"}
	mixed := formats.Report{Failed: 2, Sources: []formats.SourceOutcome{{Files: []formats.FileOutcome{
		{Decision: formats.DecisionFailed, Reason: "upload: HTTP 403 AccessDenied"},
	}}}}
	r.JudgeTick(fmt.Errorf("%w: collection stopped after 1 of 6 files", engine.ErrOffline), mixed, false, platform.Delta{})
	rec := readFailureRecord(dir)
	if rec.ConsecutiveFailures != 1 || rec.Latest() == nil || rec.Latest().Kind != formats.FailureTick {
		t.Fatalf("the AccessDenied before the outage was not counted: %+v", rec)
	}
}

func TestAStallNamesTheStageTheRunIsIn(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{eff: &config.Effective{StateDir: dir}, runID: "ffffffffffffffff"}
	r.noteStep("upload", "cursor-transcripts")
	watchFires(r, 3, 1)

	rec := readFailureRecord(dir)
	if rec.Latest() == nil || !strings.Contains(rec.Latest().Message, "tick 3 still running after") ||
		!strings.Contains(rec.Latest().Message, ", in upload cursor-transcripts") {
		t.Fatalf("the stall does not say where the run is: %+v", rec.Latest())
	}
	r.noteStep("", "")
	if r.currentStep() != "" {
		t.Fatal("the step did not clear between runs")
	}
}
