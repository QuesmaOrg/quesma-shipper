package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionDatabaseDiscoveryScope(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"state.db", "profiles/work/state.db", "cache/state.db", "lazy-packages/a/state.db", "profiles/work/auth.json"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "state.db"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "profiles", "link")); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Family: "hermes", Include: []string{"state.db", "profiles/*/state.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 2 {
		t.Fatalf("discovery: %v %+v %v", got, bad, err)
	}
	src.Exclude = []string{"profiles/**"}
	got, bad, err = sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 1 || got[0].RelPath != "state.db" {
		t.Fatalf("exclude: %v %+v %v", got, bad, err)
	}
	if err := os.Remove(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "state.db"), filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	got, bad, err = sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 0 {
		t.Fatalf("symlink escaped scope: %v %+v %v", got, bad, err)
	}
}

func TestAgentSessionRepositoryMarkers(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := repoDir(t, home, "work with spaces", true)
	filter := catalog.RepoFilter()
	for _, id := range []string{"pi-sessions", "opencode-sessions", "hermes-sessions"} {
		spec, ok := catalog.Source(id)
		if !ok {
			t.Fatal(id)
		}
		src := Resolved{Source: spec, Root: home}
		cand := Candidate{Path: filepath.Join(home, "virtual.jsonl"), CWD: project}
		if !filter.Match(src, cand) {
			t.Errorf("%s ignored repository was collected", id)
		}
	}
}

func TestSessionDatabasePatternsAndDeniedRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "custom.db"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Include: []string{"custom.db", "*.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 1 || got[0].RelPath != "custom.db" {
		t.Fatalf("YAML patterns/dedup: %v %+v %v", got, bad, err)
	}
	deny := &List{patterns: []string{normalize(root) + "/**"}}
	got, bad, err = sessionDatabases(src, deny)
	if err != nil || len(got) != 0 || bad.count != 1 || !strings.Contains(bad.reason(), "deny list refuses") {
		t.Fatalf("denied root lost diagnostic: %v %+v %v", got, bad, err)
	}
}

func TestSessionDatabaseLiteralSymlinkPrefix(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "work", "state.db"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Include: []string{"profiles/*/state.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 0 {
		t.Fatalf("literal-prefix symlink escaped scope: %v %+v %v", got, bad, err)
	}
}
