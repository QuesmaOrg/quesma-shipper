package sources

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const notrajectories = ".notrajectories"

// RepoFilter attributes candidates to the repository their session ran in and drops the
// ones whose repository carries a .notrajectories marker.
//
// Not a glob: only Claude Code encodes the working directory in the path; Codex files a
// rollout under its start date and the repository appears only in a cwd field inside the
// file. So attribution asks the file, with the catalog's own bounded head probe, and only
// for the sources that probe names.
type RepoFilter struct {
	probe *CWDProbe
	git   *GitRead
	home  string

	// cwds caches a hit per project directory (every session under an agent-encoded
	// projects/<cwd> directory shares the answer) and hits and misses per file (a sidecar
	// with no cwd field must not speak for its siblings); scopes caches the git resolution
	// per cwd. Markers are deliberately not cached: a stat is cheap, and a marker created
	// while the daemon runs must bite on the next flush, not the next restart.
	cwds   map[string]string
	scopes map[string]gitScope
}

// gitScope is the checkout containing a working directory and the repository's main
// checkout; both empty outside git, main alone empty for a bare repository.
type gitScope struct {
	root, main string
}

// RepoFilter builds the attributor from the catalog's cwd probe and the git rules of that
// same source, so the field names and what may be read stay data. With no probe nothing is
// attributed and nothing is ignored.
func (c *Compiled) RepoFilter() *RepoFilter {
	var probe *CWDProbe
	var git *GitRead
	for _, s := range c.Sources() {
		if s.CWDProbe != nil {
			probe, git = s.CWDProbe, s.GitRead
			break
		}
	}
	home, _ := os.UserHomeDir()
	return newRepoFilter(probe, git, home)
}

func newRepoFilter(probe *CWDProbe, git *GitRead, home string) *RepoFilter {
	return &RepoFilter{probe: probe, git: git, home: home, cwds: map[string]string{}, scopes: map[string]gitScope{}}
}

// CWD is the working directory a candidate's session ran in, or "" when none was found,
// which is a legal outcome.
func (f *RepoFilter) CWD(src Resolved, c Candidate) string {
	if f == nil || f.probe == nil || !slices.Contains(f.probe.From, src.ID) {
		return ""
	}
	key := c.Path
	if dir := projectDirOf(c.RelPath); dir != "" {
		key = src.Root + "\x00" + dir
		if cwd, hit := f.cwds[key]; hit {
			return cwd
		}
	}
	if cwd, hit := f.cwds[c.Path]; hit {
		return cwd
	}
	cwd := ""
	if p, ok := probeCWD(c.Path, f.probe); ok {
		cwd = cleanCWD(p)
	}
	if cwd != "" {
		f.cwds[key] = cwd
	}
	f.cwds[c.Path] = cwd
	return cwd
}

func cleanCWD(cwd string) string {
	p := filepath.Clean(strings.TrimSpace(filepath.FromSlash(cwd)))
	if p == "." || p == string(filepath.Separator) {
		return ""
	}
	return p
}

// RepoName is the last path segment, matched exactly: "acme" must never also mean
// "client-acme", because over-ignoring is silent.
func RepoName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// Marker is the .notrajectories governing dir: the nearest up to the checkout root (home
// outside git), else the main checkout's. A marker above a repository never counts.
func (f *RepoFilter) Marker(dir string) (string, bool) {
	if f == nil || dir == "" {
		return "", false
	}
	sc := f.scopeOf(dir)
	if p, ok := f.markerBetween(dir, sc.root); ok {
		return p, true
	}
	if sc.main != "" && sc.main != sc.root {
		return markerAt(sc.main)
	}
	return "", false
}

// markerBetween walks from dir up to stop inclusive; an empty stop means the walk runs to
// the home directory, and the filesystem root always ends it.
func (f *RepoFilter) markerBetween(dir, stop string) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		if p, ok := markerAt(d); ok {
			return p, true
		}
		if d == stop || d == f.home || filepath.Dir(d) == d {
			return "", false
		}
	}
}

func MarkerPath(dir string) string {
	return filepath.Join(dir, notrajectories)
}

func markerAt(dir string) (string, bool) {
	p := MarkerPath(dir)
	if _, err := os.Lstat(p); err != nil {
		return "", false
	}
	return p, true
}

// RepoDir is the directory that stands for a candidate's repository: the main working
// tree when the session ran inside a git checkout (so a worktree folds into its
// repository), the session's own working directory otherwise, "" when none was found.
func (f *RepoFilter) RepoDir(src Resolved, c Candidate) string {
	cwd := f.CWD(src, c)
	if main := f.scopeOf(cwd).main; main != "" {
		return main
	}
	return cwd
}

// A checkout at or above home (dotfiles) would claim every directory under it.
func (f *RepoFilter) scopeOf(cwd string) gitScope {
	if cwd == "" || f.git == nil {
		return gitScope{}
	}
	if sc, hit := f.scopes[cwd]; hit {
		return sc
	}
	sc := gitScope{}
	if root, common, ok := findGitDir(cwd, f.git, f.home); ok {
		sc = gitScope{root: root, main: mainWorktreeDir(common)}
	}
	f.scopes[cwd] = sc
	return sc
}

func (f *RepoFilter) Match(src Resolved, c Candidate) bool {
	if f == nil {
		return false
	}
	_, marked := f.Marker(f.CWD(src, c))
	return marked
}

// Untrack drops a marker in dir; Track removes dir's own marker. Both are idempotent, and
// Track deliberately never removes an ancestor's marker: that one may govern other
// repositories too, so lifting it is a decision to make where the file is.
func (f *RepoFilter) Untrack(dir string) error {
	return os.WriteFile(MarkerPath(dir), nil, 0o644)
}

func (f *RepoFilter) Track(dir string) error {
	if err := os.Remove(MarkerPath(dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
