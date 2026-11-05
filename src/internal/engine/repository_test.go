package engine_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// A session's manifest names the repository it ran in, a worktree's included, with remotes stripped of
// credentials; a session outside git names none.
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

	session := func(cwd string) string { return `{"type":"user","cwd":` + strconv.Quote(cwd) + "}\n" }
	f.writeTranscript("a/main.jsonl", session(repo))
	f.writeTranscript("b/worktree.jsonl", session(worktree))
	f.writeTranscript("c/notes.jsonl", session(filepath.Join(f.home, "notes")))

	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	f.runWith(func(o *engine.Options) { o.Plan.Ignore = catalog.RepoFilter() })

	user := engine.UsernameFromStateDir(f.stateDir)
	want := &formats.Repository{
		GitDir:       formats.ApplyUserPlaceholder(filepath.Join(repo, ".git"), user),
		MainWorktree: formats.ApplyUserPlaceholder(repo, user),
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
	for name, w := range map[string]*formats.Repository{"main.jsonl": want, "worktree.jsonl": want, "notes.jsonl": nil} {
		if !reflect.DeepEqual(got[name], w) {
			t.Errorf("%s: repository %+v, want %+v", name, got[name], w)
		}
	}
}
