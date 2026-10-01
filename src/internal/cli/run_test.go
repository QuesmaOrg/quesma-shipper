package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/crashjournal"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func TestRecycleMarksTheRunCleanBeforeReexec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv(app.ReexecGuardEnv, "")
	previousLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previousLimit) })

	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	var conf strings.Builder
	conf.WriteString("sources:\n")
	for _, source := range catalog.Sources() {
		fmt.Fprintf(&conf, "  - id: %s\n    enabled: false\n", source.ID)
	}
	configDir := filepath.Join(home, "config", "trajectory-shipper")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(conf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, _, err := app.LocalDev()
	if err != nil {
		t.Fatal(err)
	}
	fl, err := crashjournal.Open(paths.StateDir, "recycled-run")
	if err != nil {
		t.Fatal(err)
	}
	fl.Start()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	reexecuted := false
	err = runLoop(cmd, context.Background(), app.Build{}, false, false, false,
		fl, "recycled-run", nil,
		func(time.Time, string) bool { return true },
		func() error {
			reexecuted = true
			raw, err := os.ReadFile(filepath.Join(paths.StateDir, "crash-journal.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"ev":"exit"`) {
				t.Fatal("re-exec started before the run was marked clean")
			}
			return errors.New("synthetic re-exec failure")
		})
	if err != nil || !reexecuted {
		t.Fatalf("run loop did not recycle: err=%v reexecuted=%v", err, reexecuted)
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
