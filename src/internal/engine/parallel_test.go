package engine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

// --- the parallel pass ------------------------------------------------------
//
// Files in a source overlap; the loop thread still owns every decision. These tests pin what
// overlap must not change: the report's order, and that it overlaps. The budget and the fatal stop
// under overlap live in TestMaxFilesPerRunIsReportedNotSilent and TestARefusedAuthorizationStopsTheRun.

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
