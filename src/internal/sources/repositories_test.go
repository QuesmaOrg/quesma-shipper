package sources

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func repositoriesRequest(home string, f *Attribution) Request {
	return Request{
		Source:      Resolved{Source: Source{ID: "repositories", MaxFileBytes: 1 << 20}},
		Attribution: f, Capture: true, Context: context.Background(), Env: Env{Home: home},
		Now:      func() time.Time { return time.Date(2026, 11, 5, 10, 7, 0, 0, time.UTC) },
		Interval: 15 * time.Minute,
	}
}

// One record per repository, whatever the layout its directories take: a checkout's subdirectory, a
// worktree outside it and one already removed fold into the repository; a bare repository's worktree
// is its own; folders outside git share the last record; a marked repository is not reported.
func TestRepositoriesGroupsDirectoriesByRepository(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	writeFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://jane:ghp_secret@github.com/Acme/acme.git\n")
	sub := repoDir(t, home, "work/acme/dashboard", false)
	elsewhere := writeWorktree(t, repo, filepath.Join(home, ".t3", "worktrees"), "t3code-dc37ef43")
	gone := filepath.Join(repo, ".claude", "worktrees", "deleted")

	bare := filepath.Join(home, "work", "tool.git")
	writeFile(t, filepath.Join(bare, "config"), "[remote \"origin\"]\n\turl = git@github.com:Acme/tool.git\n")
	writeFile(t, filepath.Join(bare, "worktrees", "main", "commondir"), "../..\n")
	bareWT := filepath.Join(home, "work", "tool")
	writeFile(t, filepath.Join(bareWT, ".git"), "gitdir: "+filepath.Join(bare, "worktrees", "main")+"\n")

	plain := repoDir(t, home, "notes", false)
	marked := gitRepo(t, home, "work/secret")
	repoDir(t, home, "work/secret", true)

	root := t.TempDir()
	for i, cwd := range []string{sub, elsewhere, gone, bareWT, plain, marked} {
		writeSession(t, filepath.Join(root, "projects", "p"+string(rune('a'+i)), "s.jsonl"), cwd)
	}
	f := newAttribution(testProbe(), testGitRead(), home)
	src := Resolved{Source: Source{ID: "claude-code-transcripts", Include: []string{"projects/**/*.jsonl"}}, Root: root}
	if _, err := discoverByGlob(Request{Source: src, Attribution: f}); err != nil {
		t.Fatal(err)
	}

	d, err := Repositories{}.Discover(repositoriesRequest(home, f))
	if err != nil || len(d.Candidates) != 1 {
		t.Fatalf("discovery: %v, %d candidates", err, len(d.Candidates))
	}
	if c := d.Candidates[0]; c.RelPath != "repositories.20261105T100000Z.jsonl" || c.Series != "repositories" {
		t.Errorf("candidate %s in series %q", c.RelPath, c.Series)
	}
	p, err := d.Candidates[0].Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Bytes), "ghp_secret") || strings.Contains(string(p.Bytes), "jane") {
		t.Errorf("a remote's credentials shipped: %s", p.Bytes)
	}

	line := func(r repositoryRecord) string {
		b, _ := json.Marshal(r)
		return string(b)
	}
	want := strings.Join([]string{
		line(repositoryRecord{GitDir: filepath.Join(repo, ".git"), MainWorktree: repo,
			Remotes:     []gitRemote{{"origin", "github.com/Acme/acme"}},
			Directories: []repositoryDirectory{{Path: elsewhere}, {Path: gone, Missing: true}, {Path: sub}}}),
		line(repositoryRecord{GitDir: bare, Remotes: []gitRemote{{"origin", "github.com/Acme/tool"}},
			Directories: []repositoryDirectory{{Path: bareWT}}}),
		line(repositoryRecord{Directories: []repositoryDirectory{{Path: plain}}}),
	}, "\n")
	// Sorted by path; the .t3 worktree sorts before work/ under the same home.
	if got := strings.TrimSpace(string(p.Bytes)); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRepositoriesWithNoDirectoryShipsNothing(t *testing.T) {
	home := t.TempDir()
	d, err := Repositories{}.Discover(repositoriesRequest(home, newAttribution(testProbe(), testGitRead(), home)))
	if err != nil || len(d.Candidates) != 0 || d.Health != RootPresentNoMatch {
		t.Errorf("got %v, %d candidates, %s", err, len(d.Candidates), d.Health)
	}
}
