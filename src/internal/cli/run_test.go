package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

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

// A truncated run left backlog on disk, so the loop comes back after the catch-up delay
// rather than the full interval.
func TestTruncatedRunEarnsTheCatchUpDelay(t *testing.T) {
	// Shipped is part of the condition: a run that collected nothing has no backlog to chase.
	if got := app.NextDelay(formats.Report{Truncated: true, Shipped: 64}, nil, false, config.DefaultTick); got != app.CatchUpDelay {
		t.Fatalf("truncated run: next delay = %v, want %v", got, app.CatchUpDelay)
	}
}

func TestCompleteRunWaitsTheFullInterval(t *testing.T) {
	if got := app.NextDelay(formats.Report{}, nil, false, config.DefaultTick); got != config.DefaultTick {
		t.Fatalf("complete run: next delay = %v, want %v", got, config.DefaultTick)
	}
}

// An errored run keeps the full interval: re-ticking fast would make one failure a hot loop.
func TestErroredRunNeverEarnsTheCatchUpDelay(t *testing.T) {
	rep := formats.Report{Truncated: true}
	if got := app.NextDelay(rep, errors.New("sink unreachable"), false, config.DefaultTick); got != config.DefaultTick {
		t.Fatalf("errored run: next delay = %v, want %v", got, config.DefaultTick)
	}
}

// Whatever panicked is still on disk, so a fast re-tick would be a permanent crash loop.
func TestPanickedRunNeverEarnsTheCatchUpDelay(t *testing.T) {
	rep := formats.Report{Truncated: true}
	if got := app.NextDelay(rep, nil, true, config.DefaultTick); got != config.DefaultTick {
		t.Fatalf("panicked run: next delay = %v, want %v", got, config.DefaultTick)
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

// A run that shipped nothing has no backlog worth chasing, whatever Truncated says; per-file
// failures return no error.
func TestARunThatShippedNothingWaitsTheFullInterval(t *testing.T) {
	rep := formats.Report{Truncated: true, Failed: 64, Shipped: 0}
	if got := app.NextDelay(rep, nil, false, config.DefaultTick); got != config.DefaultTick {
		t.Fatalf("all-failures run: next delay = %v, want %v", got, config.DefaultTick)
	}
}
