package sources

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testProbe() CWDProbe {
	return CWDProbe{"claude-code-transcripts": "cwd", "codex-rollouts": "payload.cwd"}
}

func writeSession(t *testing.T, path, cwd string) {
	t.Helper()
	body := "{}\n"
	if cwd != "" {
		body = `{"type":"summary"}` + "\n" + `{"type":"user","cwd":` + strconv.Quote(cwd) + `}` + "\n"
	}
	writeBody(t, path, body)
}

// writeRollout writes Codex's envelope, which states cwd only under payload.
func writeRollout(t *testing.T, path, cwd string) {
	t.Helper()
	writeBody(t, path, `{"timestamp":"2026-07-20T09:00:00Z","type":"session_meta","payload":{"id":"s1","cwd":`+strconv.Quote(cwd)+`}}`+"\n")
}

func writeBody(t testing.TB, path, body string) {
	t.Helper()
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
func repoDir(t testing.TB, home, rel string, marked bool) string {
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
	f := newRepoFilter(testProbe(), false, home)

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

	f := newRepoFilter(testProbe(), false, home)
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
	writeRollout(t, filepath.Join(root, "sessions/2026/07/20/rollout-a.jsonl"), acme)
	writeRollout(t, filepath.Join(root, "sessions/2026/07/20/rollout-b.jsonl"), keeper)
	src := Resolved{Source: Source{ID: "codex-rollouts"}, Root: root}
	f := newRepoFilter(testProbe(), false, home)
	if !f.Match(src, candidateFor(root, "sessions/2026/07/20/rollout-a.jsonl")) {
		t.Error("the marked repository's rollout must match")
	}
	if f.Match(src, candidateFor(root, "sessions/2026/07/20/rollout-b.jsonl")) {
		t.Error("a rollout from another repository must not match")
	}
}

func TestProjectDir(t *testing.T) {
	for rel, want := range map[string]string{
		"projects/-Users-jane-acme/s.jsonl":             "-Users-jane-acme",
		"projects/-Users-jane-acme/s/subagents/a.jsonl": "-Users-jane-acme",
		"projects/s.jsonl":                              "",
		"sessions/2026/07/20/rollout-a.jsonl":           "",
		"-Users-jane-acme/agent-transcripts/c.jsonl":    "",
	} {
		if got := ProjectDir(rel); got != want {
			t.Errorf("ProjectDir(%q) = %q, want %q", rel, got, want)
		}
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
	if newRepoFilter(nil, false, home).Match(Resolved{}, c) ||
		newRepoFilter(testProbe(), false, home).Match(Resolved{Source: Source{ID: "cursor-transcripts"}, Root: root}, c) {
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
	f := newRepoFilter(testProbe(), false, home)
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
	f := newRepoFilter(testProbe(), true, home)
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

	if got := newRepoFilter(testProbe(), false, home).RepoDir(src, candidateFor(root, "projects/p-1/s.jsonl")); got != nested {
		t.Errorf("without git rules the worktree keeps its own directory, got %q", got)
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
	f := newRepoFilter(testProbe(), true, home)
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
	f := newRepoFilter(testProbe(), true, home)
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
	f := newRepoFilter(testProbe(), true, home)
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
		Ignore: newRepoFilter(testProbe(), true, home),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 0 || !d.Ignored {
		t.Errorf("want the worktree session dropped as ignored; got %d candidates, ignored=%v", len(d.Candidates), d.Ignored)
	}
}

// The filter the catalog builds reads git; without it a worktree session would be attributed
// to its disposable directory name.
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
		Ignore: newRepoFilter(testProbe(), false, home),
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
	f := newRepoFilter(testProbe(), true, home)
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
	f := newRepoFilter(testProbe(), true, home)
	if got, ok := f.Marker(wt); ok || got != "" {
		t.Fatalf("no marker yet, got %q", got)
	}
	writeFile(t, filepath.Join(repo, notrajectories), "")
	if got, ok := f.Marker(wt); !ok || got != filepath.Join(repo, notrajectories) {
		t.Errorf("Marker(worktree) = %q, %v", got, ok)
	}
}

// countProbes counts the filter's head probes.
func countProbes(f *RepoFilter) *int {
	n := new(int)
	read := f.read
	f.read = func(path, field string) (string, bool) {
		*n++
		return read(path, field)
	}
	return n
}

// statted is the candidate as discovery reports it, size and mtime included.
func statted(t *testing.T, root, rel string) Candidate {
	t.Helper()
	c := candidateFor(root, rel)
	info, err := os.Stat(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	c.Size, c.MTime = info.Size(), info.ModTime().UTC()
	return c
}

// A kept filter probes an unchanged rollout once across passes, and again once the file is
// replaced, rewritten in place, or truncated.
func TestAProbeLastsWhileTheFileHolds(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", false)
	acmf := repoDir(t, home, "work/acmf", false)
	keeper := repoDir(t, home, "work/keeper", false)
	root := t.TempDir()
	rel := "sessions/2026/07/20/rollout-a.jsonl"
	path := filepath.Join(root, rel)
	writeRollout(t, path, acme)
	src := Resolved{Source: Source{ID: "codex-rollouts"}, Root: root}
	f := newRepoFilter(testProbe(), false, home)
	probes := countProbes(f)

	look := func(want string, wantProbes int) {
		t.Helper()
		f.BeginPass()
		if got := f.CWD(src, statted(t, root, rel)); got != want || *probes != wantProbes {
			t.Fatalf("CWD = %q after %d probes, want %q after %d", got, *probes, want, wantProbes)
		}
	}
	look(acme, 1)
	look(acme, 1)

	writeRollout(t, path+".tmp", keeper)
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	look(keeper, 2)

	writeRollout(t, path, acme)
	look(acme, 3)
	// Same size: only the mtime says the head changed.
	writeRollout(t, path, acmf)
	if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	look(acmf, 4)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	look("", 5)
}

// A file no pass looks up any more is forgotten at the next one; a pass that looked nothing
// up (a paused tick) forgets nothing.
func TestAGoneFileIsSwept(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", false)
	root := t.TempDir()
	a, b := "sessions/2026/07/20/rollout-a.jsonl", "sessions/2026/07/20/rollout-b.jsonl"
	writeRollout(t, filepath.Join(root, a), acme)
	writeRollout(t, filepath.Join(root, b), acme)
	src := Resolved{Source: Source{ID: "codex-rollouts"}, Root: root}
	f := newRepoFilter(testProbe(), false, home)
	probes := countProbes(f)

	f.BeginPass()
	f.CWD(src, statted(t, root, a))
	f.CWD(src, statted(t, root, b))
	if err := os.Remove(filepath.Join(root, b)); err != nil {
		t.Fatal(err)
	}
	f.BeginPass()
	f.CWD(src, statted(t, root, a))
	f.BeginPass()
	f.BeginPass()
	if _, kept := f.files[filepath.Join(root, b)]; kept || len(f.files) != 1 {
		t.Fatalf("want only %s cached, got %d entries", a, len(f.files))
	}
	f.CWD(src, statted(t, root, a))
	if *probes != 2 {
		t.Errorf("an empty pass must not sweep a live file: %d probes, want 2", *probes)
	}
}

// Match holds a marker verdict for one pass only: a marker written or removed between two
// flushes takes effect on the second.
func TestAMarkerTakesEffectAtTheNextPass(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", false)
	root := t.TempDir()
	rel := "sessions/2026/07/20/rollout-a.jsonl"
	writeRollout(t, filepath.Join(root, rel), acme)
	src := Resolved{Source: Source{ID: "codex-rollouts"}, Root: root}
	f := newRepoFilter(testProbe(), true, home)
	c := statted(t, root, rel)

	for i, step := range []struct {
		change func() error
		want   bool
	}{
		{func() error { return nil }, false},
		{func() error { return os.WriteFile(MarkerPath(acme), nil, 0o600) }, true},
		{func() error { return os.Remove(MarkerPath(acme)) }, false},
	} {
		if err := step.change(); err != nil {
			t.Fatal(err)
		}
		f.BeginPass()
		if got := f.Match(src, c); got != step.want {
			t.Errorf("pass %d: Match = %v, want %v", i, got, step.want)
		}
	}
}

// Discovery hands the filter what it needs to keep a probe: a rollout rewritten to name a
// marked repository is probed again and dropped.
func TestDiscoveryReprobesARewrittenRollout(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	keeper := repoDir(t, home, "work/keeper", false)
	root := t.TempDir()
	path := filepath.Join(root, "sessions/2026/07/20/rollout-a.jsonl")
	writeRollout(t, path, keeper)
	f := newRepoFilter(testProbe(), true, home)
	probes := countProbes(f)
	req := Request{
		Source: Resolved{Source: Source{ID: "codex-rollouts", Include: []string{"sessions/**/*.jsonl"}}, Root: root, Enabled: true},
		Deny:   New(t.TempDir()),
		Ignore: f,
	}
	pass := func() Discovery {
		t.Helper()
		f.BeginPass()
		d, err := discoverByGlob(req)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if d := pass(); len(d.Candidates) != 1 {
		t.Fatalf("want the unmarked rollout collected, got %d", len(d.Candidates))
	}
	if pass(); *probes != 1 {
		t.Fatalf("an unchanged rollout was probed %d times, want 1", *probes)
	}
	writeRollout(t, path, acme)
	if d := pass(); len(d.Candidates) != 0 || !d.Ignored || *probes != 2 {
		t.Errorf("want the rewritten rollout probed and dropped; got %d candidates, ignored=%v, %d probes",
			len(d.Candidates), d.Ignored, *probes)
	}
}

// Consecutive discovery passes over Codex rollouts, which have no projects/<dir> to share an
// answer: "rebuilt" is a filter per flush, "kept" one filter across flushes.
func BenchmarkRepeatPassOverCodexRollouts(b *testing.B) {
	const files, repos = 3000, 20
	home, root := b.TempDir(), b.TempDir()
	instructions := strings.Repeat("x", 6<<10)
	filler := `{"timestamp":"2026-07-20T09:00:01Z","type":"response_item","payload":{"content":"` +
		strings.Repeat("y", 1000) + `"}}` + "\n"
	for i := range files {
		cwd := repoDir(b, home, fmt.Sprintf("work/repo-%d", i%repos), i%repos == 0)
		body := `{"timestamp":"2026-07-20T09:00:00Z","type":"session_meta","payload":{"id":"s","instructions":"` +
			instructions + `","cwd":` + strconv.Quote(cwd) + `}}` + "\n" + strings.Repeat(filler, 70)
		writeBody(b, filepath.Join(root, fmt.Sprintf("sessions/2026/07/%02d/rollout-%04d.jsonl", 1+i%28, i)), body)
	}
	req := Request{
		Source: Resolved{Source: Source{ID: "codex-rollouts", Include: []string{"sessions/**/*.jsonl"}}, Root: root, Enabled: true},
		Deny:   New(b.TempDir()),
	}

	for _, keep := range []bool{false, true} {
		b.Run(map[bool]string{false: "rebuilt", true: "kept"}[keep], func(b *testing.B) {
			probes := 0
			build := func() *RepoFilter {
				f := newRepoFilter(testProbe(), true, home)
				read := f.read
				f.read = func(path, field string) (string, bool) {
					probes++
					return read(path, field)
				}
				return f
			}
			f := build()
			pass := func() {
				if keep {
					f.BeginPass()
				} else {
					f = build()
				}
				req.Ignore = f
				if _, err := discoverByGlob(req); err != nil {
					b.Fatal(err)
				}
			}
			pass()
			probes, n := 0, 0
			for b.Loop() {
				pass()
				n++
			}
			b.ReportMetric(float64(probes)/float64(n), "probes/op")
		})
	}
}
