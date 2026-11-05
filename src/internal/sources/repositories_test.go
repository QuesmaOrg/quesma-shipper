package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormaliseRemoteKeepsHostAndPathOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/Acme/api.git", "github.com/Acme/api"},
		{"https://jane:ghp_secret@GitHub.com/Acme/api/", "github.com/Acme/api"},
		{"git@github.com:Acme/api.git", "github.com/Acme/api"},
		{"ssh://git@github.com:2222/Acme/api.git", "github.com/Acme/api"},
		{"git://github.com/Acme/api", "github.com/Acme/api"},
		{"github.com:Acme/api", "github.com/Acme/api"},
		{"https://gitlab.com/group/sub/api.git", "gitlab.com/group/sub/api"},
		{"git@github-work:Acme/api.git", "github-work/Acme/api"},
		{"/srv/git/api.git", ""},
		{"../api", ""},
		{`C:\repos\api`, ""},
		{"file:///srv/git/api.git", ""},
		{"https://github.com/", ""},
		{"", ""},
	} {
		if got := NormaliseRemote(tc.in); got != tc.want {
			t.Errorf("NormaliseRemote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Only remote URLs leave the config: a header carrying a token in the same file never does.
func TestGitRemotesReadsOnlyRemoteURLs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config"), `[core]
	bare = false
[remote "origin"]
	url = https://jane:ghp_secret@github.com/Acme/api.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[http]
	extraHeader = "AUTHORIZATION: bearer sekrit"
	url = https://not-a-remote.example/x
[remote "upstream"]
	URL = "git@github.com:Up/api.git" ; the canonical one
[remote.legacy]
	url = git@gitlab.com:Acme/api.git # old
[remote "local"]
	url = /srv/git/api.git
`)
	got := gitRemotes(dir)
	want := []repositoryRemote{
		{"legacy", "gitlab.com/Acme/api"},
		{"origin", "github.com/Acme/api"},
		{"upstream", "github.com/Up/api"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// One record per directory a session named, whatever the layout: a checkout's subdirectory, a
// worktree outside it, one already removed, a bare repository's worktree, a folder outside git.
// A marked repository is not reported at all.
func TestRepositoriesReportsEachSessionDirectory(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	writeFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:Acme/acme.git\n")
	sub := repoDir(t, home, "work/acme/dashboard", false)
	elsewhere := writeWorktree(t, repo, filepath.Join(home, ".t3", "worktrees"), "t3code-dc37ef43")
	gone := filepath.Join(repo, ".claude", "worktrees", "deleted")

	bare := filepath.Join(home, "work", "tool.git")
	writeFile(t, filepath.Join(bare, "config"), "[remote \"origin\"]\n\turl = https://github.com/Acme/tool\n")
	writeFile(t, filepath.Join(bare, "worktrees", "main", "commondir"), "../..\n")
	bareWT := filepath.Join(home, "work", "tool")
	writeFile(t, filepath.Join(bareWT, ".git"), "gitdir: "+filepath.Join(bare, "worktrees", "main")+"\n")

	plain := repoDir(t, home, "notes", false)
	marked := gitRepo(t, home, "work/secret")
	repoDir(t, home, "work/secret", true)

	root := t.TempDir()
	for i, cwd := range []string{sub, elsewhere, gone, bareWT, plain, marked} {
		writeSession(t, filepath.Join(root, fmt.Sprintf("projects/p-%d/s.jsonl", i)), cwd)
	}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	src := Resolved{Source: Source{ID: "claude-code-transcripts", Include: []string{"projects/**/*.jsonl"}}, Root: root}
	if _, err := discoverByGlob(Request{Source: src, Ignore: f}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 5, 10, 7, 0, 0, time.UTC)
	d, err := Repositories{}.Discover(Request{
		Source: Resolved{Source: Source{ID: "repositories", MaxFileBytes: 1 << 20}, Root: home},
		Ignore: f, Capture: true, Context: context.Background(), Env: Env{Home: home},
		Now: func() time.Time { return now }, Interval: 15 * time.Minute,
	})
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

	acme := []repositoryRemote{{"origin", "github.com/Acme/acme"}}
	want := []repositoryRecord{
		{Directory: plain},
		{Directory: bareWT, Worktree: bareWT, GitDir: bare, Remotes: []repositoryRemote{{"origin", "github.com/Acme/tool"}}},
		{Directory: gone, Missing: true, Worktree: repo, MainWorktree: repo, GitDir: filepath.Join(repo, ".git"), Remotes: acme},
		{Directory: sub, Worktree: repo, MainWorktree: repo, GitDir: filepath.Join(repo, ".git"), Remotes: acme},
		{Directory: elsewhere, Worktree: elsewhere, MainWorktree: repo, GitDir: filepath.Join(repo, ".git"), Remotes: acme},
	}
	var got []repositoryRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(p.Bytes)), "\n") {
		var r repositoryRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	byDir := func(rs []repositoryRecord) map[string]string {
		m := map[string]string{}
		for _, r := range rs {
			m[r.Directory] = fmt.Sprintf("%+v", r)
		}
		return m
	}
	g, w := byDir(got), byDir(want)
	for dir, rec := range w {
		if g[dir] != rec {
			t.Errorf("%s:\n got %s\nwant %s", dir, g[dir], rec)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d records, want %d: %s", len(got), len(want), p.Bytes)
	}
}

func TestRepositoriesWithNoDirectoryShipsNothing(t *testing.T) {
	home := t.TempDir()
	d, err := Repositories{}.Discover(Request{
		Source: Resolved{Source: Source{ID: "repositories"}, Root: home},
		Ignore: newRepoFilter(testProbe(), testGitRead(), home), Capture: true, Context: context.Background(),
		Env: Env{Home: home}, Now: time.Now, Interval: time.Minute,
	})
	if err != nil || len(d.Candidates) != 0 || d.Health != RootPresentNoMatch {
		t.Errorf("got %v, %d candidates, %s", err, len(d.Candidates), d.Health)
	}
}
