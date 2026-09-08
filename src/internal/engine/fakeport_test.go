package engine_test

// The suite's upload port: an in-memory object store behind the loop's one boundary with the network.
// A test file rather than a package, because a fake that ships in the module is a runtime adapter.
// It records every group and every stored object, so a test can assert what a run TOUCHED.

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fakeObject is one stored object.
type fakeObject struct {
	Body     []byte
	Metadata map[string]string

	// Versions stands in for a bucket's noncurrent stack: a re-PUT must add one, not replace.
	Versions int
}

type fakePort struct {
	mu      sync.Mutex
	groups  [][]string // object keys per call, in call order
	objects map[string]*fakeObject
	puts    int      // PUTs since the last reset, for "this phase wrote nothing"
	faults  []string // descriptor invariants the engine broke

	// verdict decides one object's fate; nil stores everything. call and idx are zero-based.
	verdict func(call, idx int, o engine.PreparedObject) error
	calls   int

	// FailAll fails every object, for conditions about the INSTALL rather than one object.
	FailAll error

	// FailNext fails the next object only, for the crash-before-commit and backoff paths.
	FailNext error
}

func newPort() *fakePort { return &fakePort{objects: map[string]*fakeObject{}} }

func (p *fakePort) AuthorizeAndUpload(_ context.Context, batch []engine.PreparedObject) []error {
	p.mu.Lock()
	call := p.calls
	p.calls++
	keys := make([]string, len(batch))
	ids := map[string]bool{}
	for i, o := range batch {
		keys[i] = o.Key
		switch {
		case ids[o.ObjectID]:
			p.faults = append(p.faults, "duplicate object id "+o.ObjectID)
		case o.ObjectID == "":
			p.faults = append(p.faults, "empty object id for "+o.Key)
		case !hex64.MatchString(o.SourceHash):
			p.faults = append(p.faults, "source hash is not 64 lowercase hex for "+o.Key)
		case o.Metadata["source-hash"] != "":
			p.faults = append(p.faults, "source-hash left in the metadata of "+o.Key)
		case int64(len(o.Body)) == 0:
			p.faults = append(p.faults, "empty body for "+o.Key)
		}
		ids[o.ObjectID] = true
	}
	p.groups = append(p.groups, keys)
	p.mu.Unlock()

	out := make([]error, len(batch))
	for i, o := range batch {
		if err := p.fate(call, i, o); err != nil {
			out[i] = err
			continue
		}
		p.store(o)
		out[i] = nil
	}
	return out
}

// fate is the injected failure, if any. FailAll is not consumed: a refusal keeps refusing.
func (p *fakePort) fate(call, idx int, o engine.PreparedObject) error {
	p.mu.Lock()
	if p.FailAll != nil {
		p.mu.Unlock()
		return p.FailAll
	}
	if p.FailNext != nil {
		err := p.FailNext
		p.FailNext = nil
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	if p.verdict != nil {
		return p.verdict(call, idx, o)
	}
	return nil
}

func (p *fakePort) store(o engine.PreparedObject) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.puts++
	obj := p.objects[o.Key]
	if obj == nil {
		obj = &fakeObject{}
		p.objects[o.Key] = obj
	}
	obj.Body = slices.Clone(o.Body)
	obj.Metadata = maps.Clone(o.Metadata)
	obj.Metadata["source-hash"] = o.SourceHash
	obj.Versions++
	return int64(len(obj.Body))
}

// --- test accessors ---------------------------------------------------------

// keys lists stored keys, sorted.
func (p *fakePort) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(maps.Keys(p.objects))
}

func (p *fakePort) get(key string) (*fakeObject, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	obj, ok := p.objects[key]
	return obj, ok
}

// putCount is how many objects were stored since the last reset.
func (p *fakePort) putCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.puts
}

// reset clears the recorded sequence only; stored objects stay, being the store and not the log.
func (p *fakePort) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.puts, p.groups, p.calls = 0, nil, 0
}

// sizes is how many objects each authorization group carried, in call order.
func (p *fakePort) sizes() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := make([]int, len(p.groups))
	for i, g := range p.groups {
		n[i] = len(g)
	}
	return n
}

// storedOnce fails unless every key the port holds was stored exactly once.
func (p *fakePort) storedOnce(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.faults {
		t.Errorf("descriptor invariant broken: %s", f)
	}
	for key, obj := range p.objects {
		if obj.Versions != 1 {
			t.Errorf("%s was stored %d times; one prepared object is one PUT", key, obj.Versions)
		}
	}
}
