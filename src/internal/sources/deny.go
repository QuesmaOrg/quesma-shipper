package sources

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// CompiledDeny is the floor for paths, overridable by no layer: a config whose include globs reach one of these is
// refused, not applied. Matched against expanded, symlink-resolved absolute paths, never against the glob template.
var CompiledDeny = []string{
	// Cloud and SSH credential stores.
	"~/.aws/**",
	"~/.ssh/**",
	"~/.gnupg/**",
	"~/.kube/**",
	"~/.azure/**",
	"~/.docker/config.json",
	"~/.config/gh/**",
	"~/.config/gcloud/**",
	"$APPDATA/gcloud/**",
	"$APPDATA/GitHub CLI/**",

	// Package-manager and VCS credential files.
	"~/.netrc",
	"~/.npmrc",
	"~/.pypirc",
	"~/.git-credentials",

	// Key material and environment files, anywhere.
	"**/.env",
	"**/.env.*",
	"**/id_rsa",
	"**/id_ed25519",
	"**/*.pem",
	"**/*.p12",
	"**/*.key",

	// OS keychains.
	"~/Library/Keychains/**",

	// Agent stores hold credentials that look like ordinary JSON, so collection targets projects/** and memory/**, never a whole root.
	"~/.claude/.credentials.json",
	"~/.claude.json",
	"~/.codex/auth.json",
	"~/.config/opencode/auth.json",
	"~/.local/share/opencode/auth.json",
}

type List struct {
	patterns []string
}

// New expands ~ and $VAR. Entries whose variable is unset (e.g. $APPDATA outside Windows) are dropped.
func New(home string) *List {
	d := &List{}
	env := Env{Home: home, Lookup: os.LookupEnv}
	seen := map[string]bool{}
	for _, p := range CompiledDeny {
		expanded, err := env.expandVars(p)
		if err != nil {
			continue
		}
		e := normalize(ExpandHome(expanded, home))
		if seen[e] {
			continue
		}
		seen[e] = true
		d.patterns = append(d.patterns, e)
	}
	return d
}

// Patterns returns the effective deny patterns, for `doctor` and the audit log.
func (d *List) Patterns() []string {
	return slices.Clone(d.patterns)
}

// Match reports whether an absolute path is denied, checking both the literal and the symlink-resolved form.
func (d *List) Match(path string) (bool, string) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// A path that does not exist has no resolved form, and cannot be read either.
		resolved = ""
	}
	return d.MatchPair(path, resolved)
}

// MatchPair is Match for a caller that already resolved; an empty or equal resolved form means the literal one is all there is to check.
func (d *List) MatchPair(given, resolved string) (bool, string) {
	c := normalize(given)
	if denied, pat := d.matchCandidate(c); denied {
		return true, pat
	}
	if resolved == "" {
		return false, ""
	}
	if r := normalize(resolved); r != c {
		return d.matchCandidate(r)
	}
	return false, ""
}

// MatchTree reports whether a directory is a denied tree, or sits inside one. Only a "<root>/**" pattern may prune a tree.
func (d *List) MatchTree(dir string) (bool, string) {
	c := normalize(dir)
	for _, pat := range d.patterns {
		if matchesTree(pat, c) {
			return true, pat
		}
	}
	return false, ""
}

// matchCandidate reports the first pattern in list order that matches, so doctor and the audit log quote back the compiled entry.
func (d *List) matchCandidate(c string) (bool, string) {
	for _, pat := range d.patterns {
		if ok, err := doublestar.Match(pat, c); err == nil && ok {
			return true, pat
		}
		if matchesTree(pat, c) {
			return true, pat
		}
	}
	return false, ""
}

// matchesTree is the "/**" containment the glob alone does not give: a "<root>/**" pattern denies the root itself too.
func matchesTree(pat, c string) bool {
	root, ok := strings.CutSuffix(pat, "/**")
	return ok && (c == root || strings.HasPrefix(c, root+"/"))
}

// CheckRoot refuses a collection root that is itself denied or resolves into a denied tree.
func (d *List) CheckRoot(root string) error {
	if denied, pat := d.Match(root); denied {
		return fmt.Errorf("root %s matches the compiled deny pattern %q", root, pat)
	}
	return nil
}

// CheckIncludes refuses a config whose include globs actually reach a denied file on this machine now. Glob
// subsumption is deliberately not computed: the read-time Match call stays the authority.
func (d *List) CheckIncludes(root string, includes []string) error {
	fsys := os.DirFS(root)
	for _, glob := range includes {
		pattern := strings.TrimPrefix(filepath.ToSlash(glob), "/")
		matches, err := doublestar.Glob(fsys, pattern)
		if err != nil {
			// A malformed glob is a config error in its own right.
			return fmt.Errorf("include glob %q is not valid: %w", glob, err)
		}
		for _, m := range matches {
			abs := filepath.Join(root, filepath.FromSlash(m))
			if denied, pat := d.Match(abs); denied {
				return fmt.Errorf("include glob %q reaches %s, which matches the compiled deny pattern %q",
					glob, abs, pat)
			}
		}
	}
	return nil
}

// normalize converts a path to forward slashes and, on Windows, to lower case: doublestar matching is
// slash-separated and case-sensitive.
func normalize(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
