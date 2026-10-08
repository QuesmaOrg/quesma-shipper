package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/crashjournal"
)

// The child journals a run and recycles into this same test; once it is gone, its run must read as clean.
func TestRecycleIsNotACrash(t *testing.T) {
	switch os.Getenv("QUESMA_TEST_RECYCLE") {
	case "recycle":
		fl, _, _ := startCrashJournal(io.Discard, os.Getenv("QUESMA_TEST_RECYCLE_DIR"), nil)
		os.Setenv("QUESMA_TEST_RECYCLE", "restarted")
		t.Fatal(recycle(fl))
	case "restarted":
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecycleIsNotACrash$")
	cmd.Env = append(os.Environ(), "QUESMA_TEST_RECYCLE=recycle", "QUESMA_TEST_RECYCLE_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recycle: %v, %s", err, out)
	}
	if s := crashjournal.LastRun(dir, platform.BootID()); s != nil {
		t.Fatalf("a recycle reads as a crash: %+v", s)
	}
}

func TestRecycleRetriesAFailedSupervisionProbe(t *testing.T) {
	started := time.Unix(0, 0)
	probes := 0
	serviceLoaded := func() bool {
		probes++
		return probes > 1
	}

	if recycleDue(started, started.Add(recycleAfter-time.Second), serviceLoaded) {
		t.Fatal("recycling became due before the uptime threshold")
	}
	if probes != 0 {
		t.Fatalf("supervision was probed %d times before recycling was due", probes)
	}
	if recycleDue(started, started.Add(recycleAfter), serviceLoaded) {
		t.Fatal("a failed supervision probe allowed recycling")
	}
	if !recycleDue(started, started.Add(recycleAfter+time.Second), serviceLoaded) {
		t.Fatal("a transient supervision failure permanently disabled recycling")
	}
	if probes != 2 {
		t.Fatalf("supervision was probed %d times, want 2", probes)
	}
}

// Only a clean run that shipped and was truncated left backlog on disk, so only it comes back
// after the catch-up delay rather than the full interval.
func TestNextDelay(t *testing.T) {
	for _, c := range []struct {
		name string
		rep  formats.Report
		err  error
		want time.Duration
	}{
		{"truncated run", formats.Report{Truncated: true, Shipped: 64}, nil, app.CatchUpDelay},
		{"complete run", formats.Report{}, nil, config.DefaultTick},
		// Re-ticking fast would make one failure a hot loop. A panic is the same case: recoverFlush
		// always surfaces it as an error.
		{"errored run", formats.Report{Truncated: true, Shipped: 64}, errors.New("sink unreachable"), config.DefaultTick},
		// Nothing shipped means no backlog worth chasing, whatever Truncated says; per-file
		// failures return no error.
		{"all-failures run", formats.Report{Truncated: true, Failed: 64, Shipped: 0}, nil, config.DefaultTick},
	} {
		if got := app.NextDelay(c.rep, c.err, config.DefaultTick); got != c.want {
			t.Errorf("%s: next delay = %v, want %v", c.name, got, c.want)
		}
	}
}

// The daemon must survive a panic in a tick, so the recovery is exercised through a function
// that panics rather than through the real runtime.
func TestAPanickingTickIsRecoveredAndReported(t *testing.T) {
	var errOut strings.Builder

	rep, err, panicked := recoverFlush(&errOut, nil, func() (formats.Report, error) {
		var boom *formats.Report
		return *boom, nil // nil dereference, the kind of bug this exists for
	})

	if !panicked {
		t.Fatal("a panicking tick was not reported as panicked")
	}
	if err == nil {
		t.Error("a panicking tick returned no error")
	}
	if rep.Shipped != 0 {
		t.Errorf("a panicking tick reported %d shipped", rep.Shipped)
	}
	out := errOut.String()
	if !strings.Contains(out, "PANIC in flush") {
		t.Errorf("the panic was not announced:\n%s", out)
	}
	// The stack is the whole value of recovering: without it there is no way to find the cause.
	if !strings.Contains(out, "run_test.go") {
		t.Errorf("no stack in the output:\n%s", out)
	}
}

func TestACleanTickIsUntouched(t *testing.T) {
	var errOut strings.Builder
	want := formats.Report{Shipped: 3}

	rep, err, panicked := recoverFlush(&errOut, nil, func() (formats.Report, error) {
		return want, nil
	})

	if panicked || err != nil {
		t.Fatalf("clean tick: panicked=%v err=%v", panicked, err)
	}
	if rep.Shipped != want.Shipped {
		t.Errorf("report was altered: %+v", rep)
	}
	if errOut.String() != "" {
		t.Errorf("a clean tick wrote to stderr: %q", errOut.String())
	}
}

// The journal names the source while one is in flight, the stage otherwise, and says so once the
// run has returned, so a death in the idle sleep blames no source.
func TestJournalPhaseBlamesNoSourceBetweenTicks(t *testing.T) {
	for _, tc := range []struct{ stage, source, want string }{
		{"read and seal + upload", "cursor-transcripts", "tick 7: cursor-transcripts"},
		{"heartbeat", "", "tick 7: heartbeat"},
		{"", "", "tick 7: between ticks"},
	} {
		if got := journalPhase(7, tc.stage, tc.source); got != tc.want {
			t.Errorf("journalPhase(7, %q, %q) = %q, want %q", tc.stage, tc.source, got, tc.want)
		}
	}
}
