package sources

import (
	"cmp"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// What git on disk says about a directory, read as files and never by running git: the checkout
// holding it, the repository's common git dir (shared by its worktrees) and that repository's remotes.

// GitRead is the equally bounded .git handling.
type GitRead struct {
	WalkUp           bool
	FollowGitdirFile bool
}

// findGitDir walks up from cwd to the checkout root holding .git and the repository's
// COMMON git dir: a linked worktree's own gitdir holds no config, and its commondir
// names the one that does.
func findGitDir(cwd string, cfg *GitRead, ceiling string) (root, common string, ok bool) {
	if !filepath.IsAbs(cwd) {
		return "", "", false
	}
	dir := filepath.Clean(cwd)
	for dir != ceiling {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Lstat(candidate)
		if err == nil {
			if info.IsDir() {
				return dir, candidate, true
			}
			// A worktree: .git is a file holding "gitdir: <path>".
			if cfg.FollowGitdirFile {
				if target, ok := gitPointer(candidate, "gitdir:", dir); ok {
					return dir, gitCommonDir(target), true
				}
			}
			return "", "", false
		}
		if !cfg.WalkUp {
			return "", "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
	return "", "", false
}

// gitPointer reads one of git's pointer files (a "gitdir:" line, or commondir's bare
// path) and resolves it against base.
func gitPointer(file, prefix, base string) (string, bool) {
	body, _, err := platform.ReadWhole(file, 64<<10)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(body)), prefix)
	target = filepath.FromSlash(strings.TrimSpace(target))
	if !ok || target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target), true
}

// gitCommonDir follows a linked worktree's commondir; none means this is the common dir.
func gitCommonDir(gitDir string) string {
	if common, ok := gitPointer(filepath.Join(gitDir, "commondir"), "", gitDir); ok {
		return common
	}
	return gitDir
}

// mainWorktreeDir is the checkout holding the common git dir; "" for a bare repository,
// which has no working tree to mark.
func mainWorktreeDir(commonDir string) string {
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return ""
}

// gitConfigMaxBytes bounds the read of a repository's config; a real one is a few kilobytes.
const gitConfigMaxBytes = 256 << 10

// gitRemote is one of a repository's remotes as host and path, kept apart: a path on a plain ssh
// server (/home/jane/repo) is as much the repository's name as owner/repo is on GitHub.
type gitRemote struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Path string `json:"path"`
}

// gitRemotes reads only remote.<name>.url out of a repository's config: the file can also hold
// credentials (http.extraHeader), so nothing else is taken, and an unreadable config is no remotes.
func gitRemotes(commonDir string) []gitRemote {
	body, _, err := platform.ReadWhole(filepath.Join(commonDir, "config"), gitConfigMaxBytes)
	if err != nil {
		return nil
	}
	var out []gitRemote
	remote := ""
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			remote = remoteSection(line)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if remote == "" || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if host, path := normaliseRemote(configValue(value)); host != "" {
			if r := (gitRemote{remote, host, path}); !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b gitRemote) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Host, b.Host), strings.Compare(a.Path, b.Path))
	})
	return out
}

// remoteSection is the name in [remote "name"] or the older [remote.name], "" for any other section.
func remoteSection(header string) string {
	inner, _, _ := strings.Cut(strings.TrimPrefix(header, "["), "]")
	inner = strings.TrimSpace(inner)
	if name, ok := strings.CutPrefix(inner, "remote "); ok {
		return strings.Trim(strings.TrimSpace(name), `"`)
	}
	if name, ok := strings.CutPrefix(strings.ToLower(inner), "remote."); ok {
		return name
	}
	return ""
}

// configValue drops a quoted value's quotes and an unquoted value's trailing comment.
func configValue(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, `"`); ok {
		v, _, _ = strings.Cut(rest, `"`)
		return v
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// normaliseRemote is a remote's host and path whatever the transport, so https, ssh, git and
// scp-like user@host:path name a GitHub repository alike: the URL path loses its leading slash,
// an scp path keeps what it says. The user and any token never survive. A local path or file://
// remote names no host, so both are "".
func normaliseRemote(raw string) (host, path string) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i > 0 {
		u, err := url.Parse(s)
		if err != nil || strings.EqualFold(u.Scheme, "file") {
			return "", ""
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	} else if i := strings.Index(s, ":"); i > 1 && !strings.ContainsAny(s[:i], `/\`) {
		// A single letter before the colon is a Windows drive, not a host.
		host, path = s[:i], s[i+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	} else {
		return "", ""
	}
	host = strings.ToLower(host)
	path = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return "", ""
	}
	return host, path
}
