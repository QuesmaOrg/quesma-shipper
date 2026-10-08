package crashjournal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two boots of the same machine.
const (
	bootA = "6f1c2a9e-boot-a"
	bootB = "0b7d4e31-boot-b"
)

// deadRun writes a run on bootA as a SIGKILL (an OOM kill) leaves it: entries present, no exit,
// under a PID that no longer runs.
func deadRun(t *testing.T, dir, runID string, mark func(l *Log)) {
	t.Helper()
	deadRunOn(t, dir, runID, bootA, mark)
}

func deadRunOn(t *testing.T, dir, runID, boot string, mark func(l *Log)) {
	t.Helper()
	l, err := Open(dir, runID)
	if err != nil {
		t.Fatal(err)
	}
	l.Start(boot)
	mark(l)
	stampDeadPID(t, dir, runID)
}

// stampDeadPID rewrites the run's start entry to a PID that cannot be alive, so the
// concurrent-run guard does not mistake the test process for the dead run.
func stampDeadPID(t *testing.T, dir, runID string) {
	t.Helper()
	path := filepath.Join(dir, fileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(string(raw),
		`"pid":`+pidOf(t, raw, runID), `"pid":99999999`)
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pidOf(t *testing.T, raw []byte, runID string) string {
	t.Helper()
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, runID) || !strings.Contains(line, `"pid":`) {
			continue
		}
		rest := line[strings.Index(line, `"pid":`)+len(`"pid":`):]
		if i := strings.IndexAny(rest, ",}"); i >= 0 {
			return rest[:i]
		}
	}
	t.Fatalf("no pid entry for run %s", runID)
	return ""
}

func TestCleanRunReportsNoCrash(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	l.Start(bootA)
	l.Phase("init")
	l.Exit()

	if s := LastRun(dir, bootA); s != nil {
		t.Fatalf("a clean run is no crash, got %+v", s)
	}
}

// The journal no longer names the file being read, so the phase is the finest attribution a death
// gets: enough to say a run died mid-tick rather than during startup.
func TestADeathCarriesThePhaseItReached(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) {
		l.Phase("init")
		l.Phase("tick 1")
	})

	s := LastRun(dir, bootA)
	if s == nil || s.Clean {
		t.Fatalf("want a death, got %+v", s)
	}
	if s.Phase != "tick 1" {
		t.Fatalf("want the last phase reached, got %+v", s)
	}
	if s.Crashes != 1 {
		t.Fatalf("want 1 crash, got %d", s.Crashes)
	}
}

func TestConsecutiveCrashesCounted(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("init") })
	deadRun(t, dir, "run-b", func(l *Log) { l.Phase("init") })
	deadRun(t, dir, "run-c", func(l *Log) { l.Phase("init") })

	s := LastRun(dir, bootA)
	if s == nil || s.Clean || s.RunID != "run-c" || s.Crashes != 3 || s.Phase != "init" {
		t.Fatalf("want run-c with 3 crashes at init, got %+v", s)
	}
}

// The scenario that loses the report if "previous run" is literal: a crash, then a restart that
// detected it but could not upload (exited with an error). The report must outlive that restart.
func TestCrashSurvivesErrorExitRestarts(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-crash", func(l *Log) { l.Phase("tick 1") })
	for _, id := range []string{"run-retry1", "run-retry2"} {
		l, _ := Open(dir, id)
		l.Start(bootA)
		l.Exit()
	}

	s := LastRun(dir, bootA)
	if s == nil || s.Clean || s.RunID != "run-crash" || s.Crashes != 1 {
		t.Fatalf("want run-crash still reported past two error exits, got %+v", s)
	}
	if s.Phase != "tick 1" {
		t.Fatalf("want the crashed run's phase carried, got %+v", s)
	}
}

// Only delivery clears the report: the heartbeat fails open, so a clean exit proves nothing.
func TestOnlyAReportedRunClearsTheCrash(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("init") })

	l, _ := Open(dir, "run-b")
	l.Start(bootA)
	l.Exit() // clean, but no heartbeat carrying the crash reached the sink
	if s := LastRun(dir, bootA); s == nil || s.Clean || s.RunID != "run-a" {
		t.Fatalf("a clean exit must not clear the report, got %+v", s)
	}

	l, _ = Open(dir, "run-c")
	l.Start(bootA)
	l.Reported()
	l.Exit()
	if s := LastRun(dir, bootA); s != nil {
		t.Fatalf("want the delivered report cleared, got %+v", s)
	}
}

func TestLiveConcurrentRunIsNotADeath(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, "run-live")
	l.Start(bootA) // this test's own PID: alive by definition
	l.Phase("tick 1")

	if s := LastRun(dir, bootA); s != nil {
		t.Fatalf("a live run must not report as a crash, got %+v", s)
	}
}

func TestTornLastLineTolerated(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("engine") })
	f, err := os.OpenFile(filepath.Join(dir, fileName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":"2026-01-01T00:00:00Z","run_id":"run-a","ev":"re`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s := LastRun(dir, bootA)
	if s == nil || s.Clean || s.Phase != "engine" {
		t.Fatalf("want the last whole entry to win, got %+v", s)
	}
}

func TestMissingJournal(t *testing.T) {
	if s := LastRun(t.TempDir(), bootA); s != nil {
		t.Fatalf("want nil on a fresh state dir, got %+v", s)
	}
}

func TestOpenRotatesALargeJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, make([]byte, maxLogBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "run-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("want a rotated generation: %v", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= maxLogBytes {
		t.Fatal("current file was not reset")
	}
}

func TestNilLogIsSilent(t *testing.T) {
	var l *Log
	l.Start(bootA)
	l.Phase("init")
	l.Reported()
	l.Exit()
}

// A stage entered once per upload group costs one line, not one per group.
func TestARepeatedPhaseIsOneEntry(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	l.Start(bootA)
	l.Phase("tick 1: cursor")
	l.Phase("tick 1: cursor")
	l.Phase("tick 1: cursor")
	l.Phase("tick 1: claude")
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), `"ev":"phase"`); got != 2 {
		t.Fatalf("want 2 phase entries, got %d:\n%s", got, raw)
	}
}

// A phase that failed to reach the disk is not remembered, so the retry after the journal
// recovers writes it rather than being dropped as a repeat.
func TestAFailedPhaseWriteIsRetried(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	l.Start(bootA)
	l.Phase("tick 1")
	good := l.path
	l.path = dir // a directory: the open fails
	l.Phase("tick 1: cursor")
	l.path = good
	l.Phase("tick 1: cursor")
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), `"phase":"tick 1: cursor"`); got != 1 {
		t.Fatalf("want the retried phase written once, got %d:\n%s", got, raw)
	}
}

// writeJournal stands in for a journal an earlier shipper version left behind.
func writeJournal(t *testing.T, dir string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A hard kill at shutdown (Task Scheduler on Windows, a laptop forced off) leaves no exit entry.
func TestARunStoppedWithTheMachineIsNotACrash(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("tick 3") })

	if s := LastRun(dir, bootB); s != nil {
		t.Fatalf("a run that stopped with the machine is no crash, got %+v", s)
	}
	if s := LastRun(dir, ""); s == nil || s.RunID != "run-a" {
		t.Fatalf("an unknown boot keeps the death reported, got %+v", s)
	}
}

// The boot id, not the clock, decides: a crash on this boot stays one whatever its entries say.
func TestAClockStepWithoutRebootKeepsTheCrash(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir,
		`{"at":"2020-01-01T10:00:00Z","run_id":"run-a","ev":"start","pid":99999999,"boot":"`+bootA+`"}`,
		`{"at":"2020-01-01T10:09:00Z","run_id":"run-a","ev":"phase","phase":"tick 2"}`)

	if s := LastRun(dir, bootA); s == nil || s.RunID != "run-a" || s.Phase != "tick 2" {
		t.Fatalf("want the crash on this boot reported, got %+v", s)
	}
}

// A run another run on the same boot followed died while the machine stayed up. run-c is the
// shutdown kill.
func TestAnUndeliveredCrashSurvivesALaterReboot(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("tick 2") })
	l, _ := Open(dir, "run-b")
	l.Start(bootA)
	l.Exit() // detected run-a's crash, delivered nothing
	deadRun(t, dir, "run-c", func(l *Log) { l.Phase("tick 1") })

	s := LastRun(dir, bootB)
	if s == nil || s.RunID != "run-a" || s.Crashes != 1 || s.Phase != "tick 2" {
		t.Fatalf("want run-a still reported after the reboot, counted once, got %+v", s)
	}
}

func TestOnlyDeathsOnThisBootAreCounted(t *testing.T) {
	dir := t.TempDir()
	deadRun(t, dir, "run-a", func(l *Log) { l.Phase("tick 3") })
	deadRunOn(t, dir, "run-b", bootB, func(l *Log) { l.Phase("tick 1") })

	s := LastRun(dir, bootB)
	if s == nil || s.RunID != "run-b" || s.Crashes != 1 || s.Phase != "tick 1" {
		t.Fatalf("want only the death on this boot, counted once, got %+v", s)
	}
}

// A manual sync that started while the daemon ran, both killed at shutdown.
func TestOverlappingRunsStoppedWithTheMachine(t *testing.T) {
	dir := t.TempDir()
	daemon, _ := Open(dir, "run-daemon")
	daemon.Start(bootA)
	sync, _ := Open(dir, "run-sync")
	sync.Start(bootA)
	daemon.Phase("tick 4")
	stampDeadPID(t, dir, "run-daemon")

	if s := LastRun(dir, bootB); s != nil {
		t.Fatalf("overlapping runs stopped with the machine are no crash, got %+v", s)
	}
}

func TestAJournalWithoutBootIDsKeepsReporting(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir,
		`{"at":"2026-10-01T10:00:00Z","run_id":"run-a","ev":"start","pid":99999999}`,
		`{"at":"2026-10-01T10:05:00Z","run_id":"run-a","ev":"phase","phase":"tick 2"}`,
		`{"at":"2026-10-01T10:06:00Z","run_id":"run-b","ev":"start","pid":99999999}`)

	s := LastRun(dir, bootB)
	if s == nil || s.RunID != "run-b" || s.Crashes != 2 {
		t.Fatalf("want both deaths without a boot id reported, got %+v", s)
	}
}
