package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// The runtime keeps one repo filter across flushes, and each flush is a pass on it: a marker
// written between two flushes drops the repository's rollouts from the second.
func TestAMarkerWrittenBetweenFlushesBitesOnTheNext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "work", "acme")
	root := t.TempDir()
	rollout := filepath.Join(root, "sessions", "2026", "07", "20", "rollout-a.jsonl")
	for _, dir := range []string{repo, filepath.Dir(rollout)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"type":"session_meta","payload":{"cwd":` + strconv.Quote(repo) + `}}` + "\n"
	if err := os.WriteFile(rollout, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if _, err := identity.Mint(state); err != nil {
		t.Fatal(err)
	}
	eff := &config.Effective{StateDir: state, IncludeInstallRecipient: true, Sources: []sources.Resolved{{
		Source: sources.Source{ID: "codex-rollouts", Gather: "file_glob", Include: []string{"sessions/**/*.jsonl"}},
		Root:   root, Enabled: true,
	}}}
	r, err := NewFrom(Build{}, eff, config.Paths{StateDir: state}, controlplane.Remote{})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := r.options(true).Plan.Ignore, r.options(true).Plan.Ignore; a == nil || a != b {
		t.Fatal("every flush must share the runtime's one repo filter")
	}

	flush := func() sources.HealthState {
		t.Helper()
		rep, err := r.Flush(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		return rep.Sources[0].Health
	}
	if got := flush(); got != sources.Collected {
		t.Fatalf("before the marker: health %s, want %s", got, sources.Collected)
	}
	if err := os.WriteFile(sources.MarkerPath(repo), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := flush(); got != sources.RootPresentNoMatch {
		t.Errorf("after the marker: health %s, want %s", got, sources.RootPresentNoMatch)
	}
}
