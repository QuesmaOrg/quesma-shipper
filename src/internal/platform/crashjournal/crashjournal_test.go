package crashjournal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadEntries writes a run as a SIGKILL (an OOM kill) leaves it: entries present, no exit,
// under a PID that no longer runs.
func deadRun(t *testing.T, dir, runID string, mark func(l *Log)) {
	t.Helper()
	l, err := Open(dir, runID)
	if err != nil {
		t.Fatal(err)
	}
	l.Start()
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

func TestCleanRunReportsClean(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	l.Start()
	l.Phase("init")
	l.Exit()

	s := LastRun(dir)
	if s == nil || !s.Clean || s.Crashes != 0 || s.RunID != "run-a" {
		t.Fatalf("want clean run-a, got %+v", s)
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

	s := LastRun(dir)
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

	s := LastRun(dir)
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
		l.Start()
		l.Exit()
	}

	s := LastRun(dir)
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
	l.Start()
	l.Exit() // clean, but no heartbeat carrying the crash reached the sink
	if s := LastRun(dir); s == nil || s.Clean || s.RunID != "run-a" {
		t.Fatalf("a clean exit must not clear the report, got %+v", s)
	}

	l, _ = Open(dir, "run-c")
	l.Start()
	l.Reported()
	l.Exit()
	s := LastRun(dir)
	if s == nil || !s.Clean || s.Crashes != 0 {
		t.Fatalf("want the delivered report cleared, got %+v", s)
	}
}

func TestLiveConcurrentRunIsNotADeath(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, "run-live")
	l.Start() // this test's own PID: alive by definition
	l.Phase("tick 1")

	if s := LastRun(dir); s != nil {
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

	s := LastRun(dir)
	if s == nil || s.Clean || s.Phase != "engine" {
		t.Fatalf("want the last whole entry to win, got %+v", s)
	}
}

func TestMissingJournal(t *testing.T) {
	if s := LastRun(t.TempDir()); s != nil {
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
	l.Start()
	l.Phase("init")
	l.Reported()
	l.Exit()
}
