package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

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
	return &sourcePass{budget: &b, disc: sources.Discovery{Candidates: cands}, store: newCommitBuffer(&Store{}, 0)}
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

		// A few hundred compressed bytes that decode to the whole gate hold the gate, not their stat size.
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		frame := enc.EncodeAll(make([]byte, small), nil)
		// A small first frame declares only itself; the frame after it decodes too.
		twoFrames := append(enc.EncodeAll(make([]byte, 4000), nil), frame...)
		enc.Close()
		for name, body := range map[string][]byte{"one frame": frame, "two frames": twoFrames} {
			zst := filepath.Join(t.TempDir(), "rollout.jsonl.zst")
			if err := os.WriteFile(zst, body, 0o600); err != nil {
				t.Fatal(err)
			}
			p = admissionPass(10, int64(len(body)), 1)
			p.disc.Candidates[0].Path = zst
			p.src.MaxFileBytes = 64 << 20
			if !p.canAdmit(ctx, 0, 0, &stopped) {
				t.Fatalf("%s: the .zst refused on an empty gate", name)
			}
			p.inFlightBytes += p.charge(0)
			if p.canAdmit(ctx, 1, 1, &stopped) {
				t.Errorf("%s: a %d-byte .zst decoding past the %d-byte gate let another file in beside it", name, len(body), small)
			}
		}
	})
}

// A .zst the pre-filter will skip is charged its stat size without being opened.
func TestUnchangedZstdIsChargedWithoutOpening(t *testing.T) {
	now := time.Now()
	cand := sources.Candidate{Path: filepath.Join(t.TempDir(), "gone.jsonl.zst"), Size: 100, MTime: now.Add(-48 * time.Hour)}
	shipped := Fingerprint{SourceSize: cand.Size, SourceMTime: cand.MTime, SourceHash: "shipped"}
	for _, tc := range []struct {
		name    string
		fp      *Fingerprint
		mtime   time.Time
		staging bool
		want    int64
	}{
		{"unchanged", &shipped, cand.MTime, false, cand.Size},
		{"never shipped", nil, cand.MTime, false, 1 << 20},
		{"mtime moved", &shipped, cand.MTime.Add(time.Second), false, 1 << 20},
		{"staged inside the recompute window", &Fingerprint{SourceSize: cand.Size, SourceMTime: now, SourceHash: "shipped"}, now, true, 1 << 20},
	} {
		p := admissionPass(1, cand.Size)
		p.disc.Candidates[0] = cand
		p.disc.Candidates[0].MTime = tc.mtime
		p.src.MaxFileBytes = 1 << 20
		p.o.Now = func() time.Time { return now }
		p.staging = tc.staging
		if tc.fp != nil {
			_ = p.store.Commit(KeyOf(p.src.ID, p.disc.Candidates[0]), *tc.fp)
		}
		// The path does not exist, so opening it would charge the whole cap.
		if got := p.charge(0); got != tc.want {
			t.Errorf("%s: charged %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestGeneratedFileChecksUploadStateBeforeLoading(t *testing.T) {
	for _, hash := range []string{"", "uploaded"} {
		t.Run("hash="+hash, func(t *testing.T) {
			loads := 0
			cand := sources.Candidate{Path: "snapshot.jsonl", Size: 1024, MTime: time.Unix(1000, 0), Load: func(context.Context) (sources.Payload, error) {
				loads++
				return sources.Payload{}, errors.New("fixture load failure")
			}}
			o := Options{DryRun: true, Now: time.Now}
			result, pending := o.prepareFile(context.Background(), fileJob{cand: cand, seen: true, fp: Fingerprint{SourceHash: hash, SourceSize: cand.Size, SourceMTime: cand.MTime}},
				sources.Resolved{}, sources.Discovery{}, false)
			if pending != nil {
				t.Fatal("unexpected upload")
			}
			if hash != "" {
				if loads != 0 || result.outcome.Decision != "unchanged" {
					t.Fatalf("reloaded uploaded snapshot: loads=%d, %+v", loads, result.outcome)
				}
			} else if loads != 1 || result.outcome.Decision != "parked" {
				t.Fatalf("did not retry uncommitted snapshot: loads=%d, %+v", loads, result.outcome)
			}
		})
	}
}

func TestSpentBudgetDoesNotLoadCandidates(t *testing.T) {
	p := admissionPass(0, 1024)
	p.rep, p.out = &Report{}, &SourceOutcome{}
	p.disc.Candidates[0].Load = func(context.Context) (sources.Payload, error) {
		t.Error("loaded a candidate without an upload slot")
		return sources.Payload{}, nil
	}
	if err := p.run(context.Background()); err != nil || !p.rep.Truncated || p.out.Remaining != 1 {
		t.Fatalf("budget: %+v, %v", p.out, err)
	}
}
