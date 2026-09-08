package sources

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testProbe() *CWDProbe {
	return &CWDProbe{From: []string{"claude-code-transcripts", "codex-rollouts"}, Fields: []string{"cwd", "payload.cwd"}, ScanBytes: 64 << 10}
}

func writeSession(t *testing.T, path, cwd string) {
	t.Helper()
	body := "{}\n"
	if cwd != "" {
		body = `{"type":"summary"}` + "\n" + `{"type":"user","cwd":` + strconv.Quote(cwd) + `}` + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func candidateFor(root, rel string) Candidate {
	return Candidate{Path: filepath.Join(root, rel), RelPath: rel}
}

// repoDir makes a repository directory under home, optionally carrying the marker.
func repoDir(t *testing.T, home, rel string, marked bool) string {
	t.Helper()
	dir := filepath.Join(home, rel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if marked {
		if err := os.WriteFile(filepath.Join(dir, notrajectories), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRepoNameIsTheLastSegment(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		{"/Users/jane/work/client-acme", "client-acme"},
		{"", ""},
	} {
		if got := RepoName(tc.cwd); got != tc.want {
			t.Errorf("RepoName(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

// A marker in the repository drops its sessions; a sibling with no cwd inherits its
// project directory's answer; an unmarked repository and an unattributable file ship.
func TestMarkerDropsARepositorysSessions(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	keeper := repoDir(t, home, "work/keeper", false)
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "projects/p-acme/a.jsonl"), acme)
	writeSession(t, filepath.Join(root, "projects/p-acme/b.jsonl"), "")
	writeSession(t, filepath.Join(root, "projects/p-keeper/c.jsonl"), keeper)
	writeSession(t, filepath.Join(root, "projects/p-orphan/d.jsonl"), "")
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), nil, home)

	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"projects/p-acme/a.jsonl", true},
		{"projects/p-acme/b.jsonl", true},
		{"projects/p-keeper/c.jsonl", false},
		{"projects/p-orphan/d.jsonl", false},
	} {
		if got := f.Match(src, candidateFor(root, tc.rel)); got != tc.want {
			t.Errorf("Match(%s) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// A marker covers everything under it, and the walk stops at home: a marker above home
// must not turn the whole machine off by accident.
func TestMarkerCoversDescendantsAndStopsAtHome(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, notrajectories), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(outer, "home")
	repoDir(t, home, "work", true)
	inside := repoDir(t, home, "work/acme/sub", false)
	unmarked := repoDir(t, home, "other/repo", false)

	f := newRepoFilter(testProbe(), nil, home)
	if _, ok := f.Marker(inside); !ok {
		t.Error("an ancestor's marker must cover a nested working directory")
	}
	if _, ok := f.Marker(unmarked); ok {
		t.Error("a marker above home must not count")
	}
}

// Codex files rollouts under a date directory, so attribution is per file there: one
// session must not be attributed to another's repository.
func TestMarkersDoNotLeakAcrossADateDirectory(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	keeper := repoDir(t, home, "work/keeper", false)
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "sessions/2026/07/20/rollout-a.jsonl"), acme)
	writeSession(t, filepath.Join(root, "sessions/2026/07/20/rollout-b.jsonl"), keeper)
	src := Resolved{Source: Source{ID: "codex-rollouts"}, Root: root}
	f := newRepoFilter(testProbe(), nil, home)
	if !f.Match(src, candidateFor(root, "sessions/2026/07/20/rollout-a.jsonl")) {
		t.Error("the marked repository's rollout must match")
	}
	if f.Match(src, candidateFor(root, "sessions/2026/07/20/rollout-b.jsonl")) {
		t.Error("a rollout from another repository must not match")
	}
}

// A catalog without a probe attributes nothing, and a source the probe does not name is
// never read.
func TestRepoFilterScope(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "a.jsonl"), acme)
	c := candidateFor(root, "a.jsonl")
	if newRepoFilter(nil, nil, home).Match(Resolved{}, c) ||
		newRepoFilter(testProbe(), nil, home).Match(Resolved{Source: Source{ID: "cursor-transcripts"}, Root: root}, c) {
		t.Fatal("matched with nothing to match on")
	}
}

// A marker created while the process runs bites on the next look: only attribution is
// memoised, never the marker itself.
func TestAMarkerCreatedLaterIsSeen(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", false)
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "projects/p-acme/a.jsonl"), acme)
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), nil, home)
	c := candidateFor(root, "projects/p-acme/a.jsonl")

	if f.Match(src, c) {
		t.Fatal("nothing is marked yet")
	}
	if err := f.Untrack(acme); err != nil {
		t.Fatal(err)
	}
	if !f.Match(src, c) {
		t.Error("a marker created after the first look must be seen")
	}
	if err := f.Track(acme); err != nil {
		t.Fatal(err)
	}
	if f.Match(src, c) {
		t.Error("a removed marker must stop matching")
	}
	if err := f.Track(acme); err != nil {
		t.Errorf("Track is idempotent: %v", err)
	}
}

func testGitRead() *GitRead {
	return &GitRead{WalkUp: true, FollowGitdirFile: true}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a checkout with a real .git directory.
func gitRepo(t *testing.T, home, rel string) string {
	t.Helper()
	dir := repoDir(t, home, rel, false)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeWorktree lays out a linked worktree the way git does: the pointer file in the
// worktree, the per-worktree admin dir under the main .git, and commondir pointing back.
func writeWorktree(t *testing.T, main, at, name string) string {
	t.Helper()
	adminDir := filepath.Join(main, ".git", "worktrees", name)
	writeFile(t, filepath.Join(adminDir, "commondir"), "../..\n")
	wt := filepath.Join(at, name)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+adminDir+"\n")
	return wt
}

// A session run in a worktree belongs to the repository's main checkout, not to the
// worktree directory's disposable name; a cwd outside git keeps its own directory.
func TestRepoDirIsTheMainWorktree(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	nested := writeWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees"), "woolly-kindling")
	elsewhere := writeWorktree(t, repo, filepath.Join(home, "wt"), "stray")
	gone := filepath.Join(repo, ".claude", "worktrees", "deleted")
	plain := repoDir(t, home, "notes", false)

	root := t.TempDir()
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	for i, tc := range []struct{ cwd, want string }{
		{repo, repo},
		{nested, repo},
		{filepath.Join(nested, "internal", "db"), repo},
		{elsewhere, repo},
		{gone, repo},
		{plain, plain},
	} {
		rel := fmt.Sprintf("projects/p-%d/s.jsonl", i)
		writeSession(t, filepath.Join(root, rel), tc.cwd)
		if got := f.RepoDir(src, candidateFor(root, rel)); got != tc.want {
			t.Errorf("RepoDir(cwd=%s) = %q, want %q", tc.cwd, got, tc.want)
		}
	}

	if got := newRepoFilter(testProbe(), nil, home).RepoDir(src, candidateFor(root, "projects/p-1/s.jsonl")); got != nested {
		t.Errorf("without git_read the worktree keeps its own directory, got %q", got)
	}
}

// A marker in the repository silences its worktrees wherever they live; another
// repository's worktree is untouched.
func TestMarkerInTheRepositoryDropsWorktreeSessions(t *testing.T) {
	home := t.TempDir()
	acme := gitRepo(t, home, "work/acme")
	writeFile(t, filepath.Join(acme, notrajectories), "")
	keeper := gitRepo(t, home, "work/keeper")

	root := t.TempDir()
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	for i, tc := range []struct {
		cwd  string
		want bool
	}{
		{writeWorktree(t, acme, filepath.Join(acme, ".claude", "worktrees"), "nested"), true},
		{writeWorktree(t, acme, filepath.Join(home, "wt"), "stray"), true},
		{filepath.Join(acme, ".claude", "worktrees", "deleted"), true},
		{writeWorktree(t, keeper, filepath.Join(home, "wt"), "keeper-wt"), false},
	} {
		rel := fmt.Sprintf("projects/p-%d/s.jsonl", i)
		writeSession(t, filepath.Join(root, rel), tc.cwd)
		if got := f.Match(src, candidateFor(root, rel)); got != tc.want {
			t.Errorf("Match(cwd=%s) = %v, want %v", tc.cwd, got, tc.want)
		}
	}
}

// A marker inside one worktree drops that worktree's sessions and nothing else.
func TestMarkerInAWorktreeDropsOnlyThatWorktree(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	marked := writeWorktree(t, repo, filepath.Join(home, "wt"), "marked")
	writeFile(t, filepath.Join(marked, notrajectories), "")
	open := writeWorktree(t, repo, filepath.Join(home, "wt"), "open")

	root := t.TempDir()
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	for i, tc := range []struct {
		cwd  string
		want bool
	}{{marked, true}, {open, false}, {repo, false}} {
		rel := fmt.Sprintf("projects/p-%d/s.jsonl", i)
		writeSession(t, filepath.Join(root, rel), tc.cwd)
		if got := f.Match(src, candidateFor(root, rel)); got != tc.want {
			t.Errorf("Match(cwd=%s) = %v, want %v", tc.cwd, got, tc.want)
		}
	}
}

// A repository is governed from within: a marker above the checkout root does not count
// for git sessions, while a directory outside git keeps the ancestor walk to home.
func TestMarkerAboveAGitCheckoutDoesNotCount(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "clients", notrajectories), "")
	repo := gitRepo(t, home, "clients/acme")
	wt := writeWorktree(t, repo, filepath.Join(home, "clients", "wt"), "stray")
	plain := repoDir(t, home, "clients/notes", false)

	root := t.TempDir()
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	for i, tc := range []struct {
		cwd  string
		want bool
	}{{repo, false}, {wt, false}, {plain, true}} {
		rel := fmt.Sprintf("projects/p-%d/s.jsonl", i)
		writeSession(t, filepath.Join(root, rel), tc.cwd)
		if got := f.Match(src, candidateFor(root, rel)); got != tc.want {
			t.Errorf("Match(cwd=%s) = %v, want %v", tc.cwd, got, tc.want)
		}
	}
	if _, ok := f.Marker(repo); ok {
		t.Error("Marker must not look above the checkout root")
	}
	if _, ok := f.Marker(plain); !ok {
		t.Error("Marker keeps the ancestor walk outside git")
	}
}

// Discovery itself drops a marked repository's worktree sessions: the view is not the
// only place the fold happens.
func TestDiscoveryDropsAMarkedRepositorysWorktree(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	writeFile(t, filepath.Join(repo, notrajectories), "")
	wt := writeWorktree(t, repo, filepath.Join(home, "wt"), "stray")

	root := t.TempDir()
	writeSession(t, filepath.Join(root, "projects/p-wt/s.jsonl"), wt)
	d, err := discoverByGlob(Request{
		Source: Resolved{Source: Source{ID: "claude-code-transcripts", Include: []string{"projects/**/*.jsonl"}}, Root: root, Enabled: true},
		Deny:   New(t.TempDir()),
		Ignore: newRepoFilter(testProbe(), testGitRead(), home),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 0 || !d.Ignored {
		t.Errorf("want the worktree session dropped as ignored; got %d candidates, ignored=%v", len(d.Candidates), d.Ignored)
	}
}

// The catalog wires the probe source's git_read into the filter; without it a worktree
// session would be attributed to its disposable directory name.
func TestCatalogRepoFilterReadsGit(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	wt := writeWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees"), "stray")

	root := t.TempDir()
	writeSession(t, filepath.Join(root, "projects/p-wt/s.jsonl"), wt)
	f := catalog.RepoFilter()
	if got := f.RepoDir(Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}, candidateFor(root, "projects/p-wt/s.jsonl")); got != repo {
		t.Errorf("RepoDir = %q, want %q", got, repo)
	}
}

// A source emptied by the markers says so, and is not the drift state; an oversized file
// in an untracked repository is not reported by name either.
func TestDiscoveryDistinguishesIgnoredFromDrift(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "projects/p-acme/a.jsonl"), acme)
	writeSession(t, filepath.Join(root, "projects/p-acme/b.jsonl"), "")
	d, err := discoverByGlob(Request{
		Source: Resolved{Source: Source{ID: "claude-code-transcripts", Include: []string{"projects/**/*.jsonl"}, MaxFileBytes: 4}, Root: root, Enabled: true},
		Deny:   New(t.TempDir()),
		Ignore: newRepoFilter(testProbe(), nil, home),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 0 || len(d.Oversize) != 0 || !d.Ignored || !strings.Contains(d.Reason, "not tracking") {
		t.Errorf("want an empty, deliberately ignored source; got %d candidates, %d oversize, ignored=%v, reason %q",
			len(d.Candidates), len(d.Oversize), d.Ignored, d.Reason)
	}
}

func TestACheckoutAtHomeDoesNotClaimEverything(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	notes := repoDir(t, home, "notes", false)
	acme := gitRepo(t, home, "work/acme")

	root := t.TempDir()
	src := Resolved{Source: Source{ID: "claude-code-transcripts"}, Root: root}
	f := newRepoFilter(testProbe(), testGitRead(), home)
	for i, tc := range []struct{ cwd, want string }{{notes, notes}, {home, home}, {acme, acme}} {
		rel := fmt.Sprintf("projects/p-%d/s.jsonl", i)
		writeSession(t, filepath.Join(root, rel), tc.cwd)
		if got := f.RepoDir(src, candidateFor(root, rel)); got != tc.want {
			t.Errorf("RepoDir(cwd=%s) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
	writeFile(t, filepath.Join(home, notrajectories), "")
	if _, ok := f.Marker(notes); !ok {
		t.Error("a marker at home should cover a plain directory under it")
	}
	if _, ok := f.Marker(acme); ok {
		t.Error("a marker at home must not reach into a checkout")
	}
}

func TestMarkerOnAWorktreeIsTheRepositorys(t *testing.T) {
	home := t.TempDir()
	repo := gitRepo(t, home, "work/acme")
	wt := writeWorktree(t, repo, filepath.Join(home, "wt"), "stray")
	f := newRepoFilter(testProbe(), testGitRead(), home)
	if got, ok := f.Marker(wt); ok || got != "" {
		t.Fatalf("no marker yet, got %q", got)
	}
	writeFile(t, filepath.Join(repo, notrajectories), "")
	if got, ok := f.Marker(wt); !ok || got != filepath.Join(repo, notrajectories) {
		t.Errorf("Marker(worktree) = %q, %v", got, ok)
	}
}
