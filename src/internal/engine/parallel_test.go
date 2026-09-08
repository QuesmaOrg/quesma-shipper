package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// --- the parallel pass ------------------------------------------------------
//
// Files in a source overlap; the loop thread still owns every decision. These tests pin what
// overlap must not change: the budget, the report's order, the fatal stop, and that it overlaps.

// Budget is reserved at admission, so no number of goroutines can overshoot max_files_per_run.
func TestTheBudgetIsNotOvershotByFilesInFlight(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.writeTranscript(fmt.Sprintf("p/a%02d.jsonl", i), line1)
	}
	f.eff.MaxFilesPerRun = 2

	rep := f.runWith(func(o *engine.Options) { o.Workers = 8 })

	if rep.Shipped != 2 {
		t.Errorf("budget 2 shipped %d files", rep.Shipped)
	}
	if !rep.Truncated {
		t.Error("a run that left 18 files behind did not say so")
	}
	if got := len(f.port.keys()); got != 2 {
		t.Errorf("%d objects reached the store; the budget allows 2", got)
	}
}

// out.Files is index-addressed, so the report reads in candidate order however work interleaved.
func TestTheReportKeepsCandidateOrderHoweverTheWorkFinished(t *testing.T) {
	f := newFixture(t)
	var want []string
	for i := 0; i < 12; i++ {
		rel := fmt.Sprintf("p/o%02d.jsonl", i)
		f.writeTranscript(rel, line1)
		want = append(want, "projects/"+rel)
	}

	rep := f.runWith(func(o *engine.Options) { o.Workers = 8 })

	files := rep.Sources[0].Files
	if len(files) != len(want) {
		t.Fatalf("want %d files in the report, got %d", len(want), len(files))
	}
	for i, fo := range files {
		if fo.RelPath != want[i] {
			t.Errorf("position %d: want %s, got %s", i, want[i], fo.RelPath)
		}
	}
}

// The refusal stops ADMISSION, not just the count, measured in authorization calls.
func TestARefusedInstallDoesNotAttemptEveryFile(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.writeTranscript(fmt.Sprintf("p/r%02d.jsonl", i), line1)
	}
	f.port.FailAll = fmt.Errorf("creds: vend failed: %w", formats.ErrCredentialsRefused)

	o := f.opts()
	o.Workers = 4
	rep, err := engine.Run(context.Background(), f.store, o)

	if !errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatalf("want a refusal error from the run, got %v", err)
	}
	if rep.Failed != 1 {
		t.Errorf("%d refusals counted; duplicates in flight are the same fact about the same install", rep.Failed)
	}
	if got := f.port.putCount(); got != 0 {
		t.Errorf("%d objects stored against a revoked install", got)
	}
	if got := len(f.port.sizes()); got >= 20 {
		t.Errorf("%d authorizations against a revoked install; admission never stopped", got)
	}
}

// slowRecipient counts how many objects are sealed at once and holds each open long enough for
// overlap to be observable. It instruments the compute leg; the port would measure batching.
type slowRecipient struct {
	inner age.Recipient
	mu    sync.Mutex
	cur   int
	peak  int
}

func (r *slowRecipient) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	r.mu.Lock()
	r.cur++
	if r.cur > r.peak {
		r.peak = r.cur
	}
	r.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	st, err := r.inner.Wrap(fileKey)
	r.mu.Lock()
	r.cur--
	r.mu.Unlock()
	return st, err
}

func (r *slowRecipient) Peak() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// slowPort counts authorization groups in flight, the first blocking on a barrier until a second
// arrives: only a network leg folded back into a compute slot trips the timeout.
type slowPort struct {
	*fakePort
	mu   sync.Mutex
	cur  int
	peak int
	gate chan struct{}
	once sync.Once
}

func (p *slowPort) AuthorizeAndUpload(ctx context.Context, batch []engine.PreparedObject) []error {
	p.mu.Lock()
	if p.gate == nil {
		p.gate = make(chan struct{})
	}
	gate := p.gate
	p.cur++
	if p.cur > p.peak {
		p.peak = p.cur
	}
	reached := p.cur >= 2
	p.mu.Unlock()

	if reached {
		p.once.Do(func() { close(gate) })
	} else {
		// The timeout is the failure path: an overlapping port releases as the second group arrives.
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
	}

	out := p.fakePort.AuthorizeAndUpload(ctx, batch)
	p.mu.Lock()
	p.cur--
	p.mu.Unlock()
	return out
}

func (p *slowPort) Peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// The one test that would notice the pool silently reduced to a sequential loop.
func TestFilesAreProcessedConcurrently(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 8; i++ {
		f.writeTranscript(fmt.Sprintf("p/c%02d.jsonl", i), line1)
	}
	slow := &slowRecipient{inner: f.unit.Recipient()}

	rep := f.runWith(func(o *engine.Options) {
		o.Workers = 4
		o.Recipients = []age.Recipient{slow}
	})

	if rep.Shipped != 8 {
		t.Fatalf("want 8 shipped, got %+v", rep)
	}
	if slow.Peak() < 2 {
		t.Errorf("peak concurrent seals %d; the pass ran sequentially", slow.Peak())
	}
}

// Authorization groups are not bound by the compute pool: a sealed object leaves its compute slot
// before its group touches the network. The 70 files and the wide upload pool put two in flight.
func TestUploadsOverlapBeyondTheComputePool(t *testing.T) {
	f := newFixture(t)
	const files = 70
	for i := 0; i < files; i++ {
		f.writeTranscript(fmt.Sprintf("p/u%02d.jsonl", i), line1)
	}
	f.eff.MaxFilesPerRun = files
	port := &slowPort{fakePort: f.port}

	rep := f.runWith(func(o *engine.Options) {
		o.Plan = planOf(f.eff)
		o.Workers = 2
		o.UploadWorkers = 64
		o.Upload = port
	})

	if rep.Shipped != files {
		t.Fatalf("want %d shipped, got %+v", files, rep)
	}
	if port.Peak() < 2 {
		t.Errorf("peak concurrent authorizations %d with 2 compute workers; groups are still holding compute slots", port.Peak())
	}
}

// Workers=1, UploadWorkers=1 pins both pools to one file: same decisions, same order, same
// second-run silence as a sequential loop.
func TestOneWorkerBehavesExactlyLikeTheOldLoop(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.writeTranscript(fmt.Sprintf("p/s%02d.jsonl", i), line1)
	}

	pin := func(o *engine.Options) { o.Workers, o.UploadWorkers = 1, 1 }
	rep := f.runWith(pin)
	if rep.Shipped != 6 || rep.Failed != 0 {
		t.Fatalf("first pass: want 6 shipped, got %+v", rep)
	}

	again := f.runWith(pin)
	if again.Unchanged != 6 || again.Shipped != 0 {
		t.Errorf("second pass: want 6 unchanged, got %+v", again)
	}
}
