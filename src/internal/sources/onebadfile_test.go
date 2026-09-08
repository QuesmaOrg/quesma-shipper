package sources_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// One unreadable file must not silence the whole source: the sniff asks about the STORE, so it samples more than one file.
func TestOneUnreadableFileDoesNotSilenceTheSource(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The oldest file is the broken one.
	bad := filepath.Join(dir, "00000000-0000-4000-8000-000000000000.jsonl")
	if err := os.WriteFile(bad, []byte("\x00\x00\x00 not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	older(t, bad)

	for _, name := range []string{"11111111", "22222222", "33333333"} {
		good := filepath.Join(dir, name+"-1111-4111-8111-111111111111.jsonl")
		if err := os.WriteFile(good, []byte(`{"type":"user","uuid":"u1"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d := discover(t, source(filepath.Join(home, ".claude"), []string{"projects/**/*.jsonl"}), nil)

	if d.Health != sources.Collected {
		t.Errorf("health %q (%s) — one bad file is not a broken source", d.Health, d.Reason)
	}
	if len(d.Candidates) != 4 {
		t.Errorf("got %d candidates, want all 4: the bad one fails per-file, not source-wide",
			len(d.Candidates))
	}
	if d.SniffFailures == 0 {
		t.Error("the bad file was not noticed at all; it should be counted even when the source is fine")
	}
}

// The source-wide verdict must still exist: a store that really did change substrate has to stop collection.
func TestASourceWhereEveryFileIsUnreadableIsStillCondemned(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"11111111", "22222222", "33333333"} {
		p := filepath.Join(dir, name+"-1111-4111-8111-111111111111.jsonl")
		if err := os.WriteFile(p, []byte("\x00\x00\x00 encrypted now\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d := discover(t, source(filepath.Join(home, ".claude"), []string{"projects/**/*.jsonl"}), nil)

	if d.Health == sources.Collected {
		t.Error("every file is unreadable and the source reports collected")
	}
	if len(d.Candidates) != 0 {
		t.Errorf("%d candidates from a store that cannot be read", len(d.Candidates))
	}
}

func older(t *testing.T, path string) {
	t.Helper()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
