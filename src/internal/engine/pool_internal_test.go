package engine

import (
	"context"
	"strconv"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func TestConcurrencyTakesThePinAndNeverExceedsTheCandidates(t *testing.T) {
	cases := []struct {
		workers, candidates, want int
	}{
		{3, 100, 3}, // a test pin wins
		{8, 2, 2},   // never more goroutines than files
		{0, 0, 1},   // and never fewer than one
	}
	for _, c := range cases {
		o := Options{Workers: c.workers}
		if got := o.concurrency(c.candidates); got != c.want {
			t.Errorf("concurrency(workers=%d, candidates=%d) = %d, want %d",
				c.workers, c.candidates, got, c.want)
		}
	}
}

func TestUploadConcurrencyTakesThePin(t *testing.T) {
	if got := (Options{UploadWorkers: 5}).uploadConcurrency(3); got != 5 {
		t.Errorf("uploadConcurrency(uploadWorkers=5, compute=3) = %d, want 5", got)
	}
}

func admissionPass(budget int, sizes ...int64) *sourcePass {
	cands := make([]sources.Candidate, len(sizes))
	for i, sz := range sizes {
		cands[i] = sources.Candidate{Size: sz}
	}
	b := budget
	return &sourcePass{budget: &b, disc: sources.Discovery{Candidates: cands}}
}

// Each gate alone: the admission predicate is the whole budget-and-safety policy of the pass.
func TestAdmissionGates(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	t.Run("budget is reserved by admission", func(t *testing.T) {
		p := admissionPass(1, 1, 1)
		var stopped error
		if !p.canAdmit(ctx, 0, 0, &stopped) {
			t.Fatal("first admission refused with budget available")
		}
		if p.canAdmit(ctx, 1, 1, &stopped) {
			t.Error("second admission granted on a spent budget")
		}
		if *p.budget != 0 {
			t.Errorf("budget = %d after one admission from 1", *p.budget)
		}
	})

	t.Run("a fatal pass admits nothing", func(t *testing.T) {
		p := admissionPass(10, 1, 1)
		p.fatal = true
		var stopped error
		if p.canAdmit(ctx, 0, 0, &stopped) {
			t.Error("admitted a file after a refusal")
		}
	})

	t.Run("a cancelled context stops admission and says so once", func(t *testing.T) {
		p := admissionPass(10, 1, 1)
		var stopped error
		if p.canAdmit(cancelled, 0, 0, &stopped) {
			t.Error("admitted a file on a dead context")
		}
		if stopped == nil {
			t.Error("the cancellation was not recorded for the pass to return")
		}
	})

	t.Run("in-flight bytes hold a large file back", func(t *testing.T) {
		p := admissionPass(10, platform.MaxInFlightBytes(), platform.MaxInFlightBytes())
		var stopped error
		if !p.canAdmit(ctx, 0, 0, &stopped) {
			t.Fatal("first large file refused")
		}
		p.inFlightBytes = p.disc.Candidates[0].Size
		if p.canAdmit(ctx, 1, 1, &stopped) {
			t.Error("two gate-sized files admitted together")
		}
	})

	t.Run("a file larger than the whole gate still runs, alone", func(t *testing.T) {
		p := admissionPass(10, platform.MaxInFlightBytes()*2)
		var stopped error
		if !p.canAdmit(ctx, 0, 0, &stopped) {
			t.Error("an oversized file was refused outright; it must run with the pass to itself")
		}
	})

	// The gate is read at admission, not baked in at compile time.
	t.Run("the gate follows the environment override", func(t *testing.T) {
		const small = 4 << 20
		t.Cleanup(func() {
			if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
				t.Fatalf("restoring the cap: %v", err)
			}
		})
		t.Setenv(platform.EnvMaxInFlightBytes, strconv.Itoa(small))
		if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
			t.Fatalf("applying the cap: %v", err)
		}

		p := admissionPass(10, small, small)
		var stopped error
		if !p.canAdmit(ctx, 0, 0, &stopped) {
			t.Fatal("first file refused under the overridden cap")
		}
		p.inFlightBytes = p.disc.Candidates[0].Size
		if p.canAdmit(ctx, 1, 1, &stopped) {
			t.Error("two files admitted together past the overridden cap")
		}
	})
}
