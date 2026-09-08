package engine

// The authorization accumulator, exercised directly: the byte bound needs objects too large to
// produce through the loop, and overshooting it loses a whole group.

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// sizePort records the ciphertext each group carried and stores everything.
type sizePort struct {
	mu     sync.Mutex
	groups [][]int
}

func (p *sizePort) AuthorizeAndUpload(_ context.Context, batch []PreparedObject) []error {
	sizes := make([]int, len(batch))
	for i, o := range batch {
		sizes[i] = len(o.Body)
	}
	p.mu.Lock()
	p.groups = append(p.groups, sizes)
	p.mu.Unlock()
	return make([]error, len(batch))
}

// stagedFor is one sealed object of a given size.
func stagedFor(idx, size int) fileResult {
	return fileResult{
		idx:   idx,
		bytes: int64(size),
		pending: &pendingPut{
			key:       Key{SourceID: "s", NativePath: fmt.Sprintf("/f%d", idx)},
			objectKey: fmt.Sprintf("v1/o/%d.age", idx),
			obj:       make([]byte, size),
			md:        map[string]string{"source-hash": "deadbeef"},
		},
	}
}

// run stages every size and drains every group the accumulator produced.
func stageAll(t *testing.T, sizes []int) [][]int {
	t.Helper()
	ctx := context.Background()
	port := &sizePort{}
	p := &sourcePass{o: Options{Upload: port}}
	// Buffered past the group count, so the accumulator's send never blocks the staging loop.
	batches := make(chan []fileResult, len(sizes)+1)
	p.staged = &batcher[stagedUpload]{
		maxObjects: maxBatchObjects,
		send:       func(items []stagedUpload) { batches <- p.o.sendBatch(ctx, items) },
	}

	for i, sz := range sizes {
		if done := p.stageUpload(stagedFor(i, sz)); len(done) != 0 {
			t.Fatalf("object %d was not staged: %+v", i, done[0].outcome)
		}
	}
	p.staged.flush()
	for seen := 0; seen < len(sizes); {
		seen += len(<-batches)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.groups
}

// Both bounds hold at once, and every object is authorized exactly once across the groups.
func TestTheAuthorizationGroupIsBoundedByBytesAndByCount(t *testing.T) {
	tiny := make([]int, 70)
	for i := range tiny {
		tiny[i] = 1 << 10
	}
	cases := []struct {
		name  string
		sizes []int
	}{
		{"count", tiny},
		{"bytes", []int{40 << 20, 30 << 20, 10 << 20}},
		{"one object over the whole bound rides alone", []int{70 << 20, 1 << 10}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			groups := stageAll(t, c.sizes)
			objects := 0
			for _, g := range groups {
				bytes := 0
				for _, sz := range g {
					bytes += sz
				}
				if len(g) > maxBatchObjects {
					t.Errorf("a group carried %d objects, over the %d bound", len(g), maxBatchObjects)
				}
				// An object larger than the whole bound is the one exception, and it rides alone.
				if bytes > maxBatchBytes && len(g) != 1 {
					t.Errorf("a group of %d carried %d bytes, over the %d bound",
						len(g), bytes, maxBatchBytes)
				}
				objects += len(g)
			}
			if objects != len(c.sizes) {
				t.Errorf("%d objects authorized for %d staged", objects, len(c.sizes))
			}
		})
	}
}

// source-hash leaves the metadata map and becomes the descriptor's own field.
func TestSourceHashIsLiftedOutOfTheMetadata(t *testing.T) {
	obj := preparedFrom(0, "k", nil, map[string]string{
		"source-hash":  "abc",
		"source-id":    "claude-code-transcripts",
		"shipped-hash": "def",
	})
	if obj.SourceHash != "abc" {
		t.Errorf("source hash = %q", obj.SourceHash)
	}
	if _, ok := obj.Metadata["source-hash"]; ok {
		t.Error("source-hash survived in the request metadata")
	}
	if len(obj.Metadata) != 2 {
		t.Errorf("metadata lost or gained names: %v", obj.Metadata)
	}
	if preparedFrom(0, "k", nil, nil).Metadata == nil {
		t.Error("nil metadata produced a nil map rather than an empty one")
	}
}
