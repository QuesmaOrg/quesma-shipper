package sources_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// An oversized file must be skipped AND counted, or it is excluded in silence while the source still reports collected.
func TestAnOversizedFileIsSkippedAndCounted(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-Users-dev-work-demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "11111111-1111-4111-8111-111111111111.jsonl")
	big := filepath.Join(dir, "22222222-2222-4222-8222-222222222222.jsonl")
	if err := os.WriteFile(small, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sparse: the cap is checked against the stat, so a file this size must never be read.
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(300 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	src := source(filepath.Join(home, ".claude"), []string{"projects/**/*.jsonl"})
	src.MaxFileBytes = 256 << 20
	disc := discover(t, src, nil)

	if got := len(disc.Candidates); got != 1 {
		t.Fatalf("want the one small file as a candidate, got %d", got)
	}
	if strings.Contains(disc.Candidates[0].RelPath, "22222222") {
		t.Fatal("the oversized file became a candidate")
	}
	if len(disc.Oversize) != 1 {
		t.Fatalf("want one oversized file recorded, got %d", len(disc.Oversize))
	}
	if disc.Oversize[0].Size <= disc.Oversize[0].Limit {
		t.Errorf("recorded size %d is not over the limit %d",
			disc.Oversize[0].Size, disc.Oversize[0].Limit)
	}
	// The source is still healthy: one bad file must not stop the other four hundred.
	if disc.Health != sources.Collected {
		t.Errorf("health is %q, want collected — one oversized file is not a broken source",
			disc.Health)
	}
}
