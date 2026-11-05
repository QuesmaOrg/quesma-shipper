package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// A session's manifest names the repository it ran in, a worktree's included, with remotes stripped of
// credentials and one git_dir however the checkout was reached; a session outside git, or in a
// worktree whose repository is gone, names none.
func TestManifestNamesTheSessionRepository(t *testing.T) {
	f := newFixture(t)
	t.Setenv("HOME", f.home)
	repo := filepath.Join(f.home, "work", "api")
	writeFile(t, filepath.Join(repo, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://jane:ghp_secret@github.com/Acme/api.git\n"))
	admin := filepath.Join(repo, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(admin, "commondir"), []byte("../..\n"))
	worktree := filepath.Join(f.home, "wt")
	writeFile(t, filepath.Join(worktree, ".git"), []byte("gitdir: "+admin+"\n"))
	orphan := filepath.Join(f.home, "orphan")
	writeFile(t, filepath.Join(orphan, ".git"), []byte("gitdir: "+filepath.Join(f.home, "gone", ".git", "worktrees", "x")+"\n"))

	session := func(cwd string) string { return `{"type":"user","cwd":` + jsonString(cwd) + "}\n" }
	f.writeTranscript("a/main.jsonl", session(repo))
	f.writeTranscript("b/worktree.jsonl", session(worktree))
	f.writeTranscript("c/notes.jsonl", session(filepath.Join(f.home, "notes")))
	f.writeTranscript("d/orphan.jsonl", session(orphan))
	sessions := map[string]bool{"main.jsonl": true, "worktree.jsonl": true}
	// Windows needs a privilege for symlinks, so there the linked checkout goes untested.
	if err := os.Symlink(filepath.Join(f.home, "work"), filepath.Join(f.home, "code")); err == nil {
		f.writeTranscript("e/linked.jsonl", session(filepath.Join(f.home, "code", "api")))
		sessions["linked.jsonl"] = true
	}

	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	f.runWith(func(o *engine.Options) { o.Plan.Ignore = catalog.RepoFilter() })

	user := engine.UsernameFromStateDir(f.stateDir)
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := &formats.Repository{
		GitDir:       formats.ApplyUserPlaceholder(filepath.Join(resolved, ".git"), user),
		MainWorktree: formats.ApplyUserPlaceholder(resolved, user),
		Remotes:      []formats.Remote{{Name: "origin", Host: "github.com", Path: "Acme/api"}},
	}
	got := map[string]*formats.Repository{}
	for _, k := range f.port.keys() {
		m, _ := f.plaintext(k)
		got[filepath.Base(m.NativePath)] = m.Repository
		if raw, _ := json.Marshal(m); strings.Contains(string(raw), "ghp_secret") {
			t.Errorf("%s: the remote's token reached the manifest", m.NativePath)
		}
	}
	for name := range sessions {
		if !reflect.DeepEqual(got[name], want) {
			t.Errorf("%s: repository %+v, want %+v", name, got[name], want)
		}
	}
	for _, name := range []string{"notes.jsonl", "orphan.jsonl"} {
		if got[name] != nil {
			t.Errorf("%s: repository %+v, want none", name, got[name])
		}
	}
}
