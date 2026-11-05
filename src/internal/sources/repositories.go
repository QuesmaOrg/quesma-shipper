package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Repositories reports every working directory this flush's sessions named, with the git checkout
// holding it and that repository's remotes as host/path. It states facts per directory and leaves
// which directories form one repository to the reader, so a remote added later re-links old sessions.
type Repositories struct{}

// RepositoriesGather runs after every other source: the directories it reports are the ones their
// discovery attributed.
const RepositoriesGather = "repositories"

type repositoryRecord struct {
	Directory string `json:"directory"`
	// Missing: the directory is gone (a removed worktree, a remote machine's path), so the git
	// fields, if any, come from the nearest checkout above it.
	Missing      bool               `json:"missing,omitempty"`
	Worktree     string             `json:"worktree,omitempty"`
	MainWorktree string             `json:"main_worktree,omitempty"`
	GitDir       string             `json:"git_dir,omitempty"`
	Remotes      []repositoryRemote `json:"remotes,omitempty"`
}

type repositoryRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// gitConfigMaxBytes bounds the read of a repository's config; a real one is a few kilobytes.
const gitConfigMaxBytes = 256 << 10

func (Repositories) Discover(req Request) (Discovery, error) {
	d, bucket, ready, err := generatedDiscovery(req, "repositories")
	if !ready {
		return d, err
	}
	scopes := req.Ignore.observed()
	if len(scopes) == 0 {
		d.Reason = "no collected session named a working directory"
		return d, nil
	}
	name := "repositories." + bucket.Format("20060102T150405Z") + ".jsonl"
	d.Candidates = []Candidate{{Path: name, RelPath: name, Series: req.Source.ID, Size: req.Source.MaxFileBytes, MTime: bucket,
		Load: func(ctx context.Context) (Payload, error) { return loadRepositories(ctx, req, scopes) },
	}}
	d.Health = Collected
	return d, nil
}

type observedDir struct {
	dir   string
	scope gitScope
}

// observed is every directory the filter attributed, sorted, minus the ones a marker keeps out.
func (f *RepoFilter) observed() []observedDir {
	if f == nil {
		return nil
	}
	var out []observedDir
	for dir, sc := range f.scopes {
		if _, marked := f.Marker(dir); !marked {
			out = append(out, observedDir{dir, sc})
		}
	}
	slices.SortFunc(out, func(a, b observedDir) int { return strings.Compare(a.dir, b.dir) })
	return out
}

func loadRepositories(ctx context.Context, req Request, dirs []observedDir) (Payload, error) {
	user := func(p string) string { return formats.ApplyUserPlaceholder(p, req.Username) }
	remotes := map[string][]repositoryRemote{}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, o := range dirs {
		if err := ctx.Err(); err != nil {
			return Payload{}, err
		}
		rec := repositoryRecord{Directory: user(o.dir), Worktree: user(o.scope.root), MainWorktree: user(o.scope.main), GitDir: user(o.scope.common)}
		if _, err := os.Lstat(o.dir); err != nil {
			rec.Missing = true
		}
		if o.scope.common != "" {
			rs, seen := remotes[o.scope.common]
			if !seen {
				rs = gitRemotes(o.scope.common)
				remotes[o.scope.common] = rs
			}
			rec.Remotes = rs
		}
		if err := enc.Encode(rec); err != nil {
			return Payload{}, err
		}
	}
	if req.Source.MaxFileBytes > 0 && int64(b.Len()) > req.Source.MaxFileBytes {
		return Payload{}, fmt.Errorf("%w: generated content exceeds file size limit", platform.ErrTooLarge)
	}
	return Payload{Bytes: []byte(b.String())}, nil
}

// gitRemotes reads only remote.<name>.url out of a repository's config: the file can also hold
// credentials (http.extraHeader), so nothing else is taken, and an unreadable config is no remotes.
func gitRemotes(commonDir string) []repositoryRemote {
	body, _, err := platform.ReadWhole(filepath.Join(commonDir, "config"), gitConfigMaxBytes)
	if err != nil {
		return nil
	}
	var out []repositoryRemote
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
		if u := NormaliseRemote(configValue(value)); u != "" {
			r := repositoryRemote{Name: remote, URL: u}
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b repositoryRemote) int {
		return strings.Compare(a.Name+"\x00"+a.URL, b.Name+"\x00"+b.URL)
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

// NormaliseRemote is a remote as host/path whatever the transport: https, ssh, git and scp-like
// user@host:path all name the same repository, and the user and any token never survive. A local
// path or file:// remote names no host, so it is "".
func NormaliseRemote(raw string) string {
	s := strings.TrimSpace(raw)
	var host, path string
	if i := strings.Index(s, "://"); i > 0 {
		u, err := url.Parse(s)
		if err != nil || strings.EqualFold(u.Scheme, "file") {
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else if i := strings.Index(s, ":"); i > 1 && !strings.ContainsAny(s[:i], `/\`) {
		// A single letter before the colon is a Windows drive, not a host.
		host, path = s[:i], s[i+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	} else {
		return ""
	}
	host = strings.ToLower(host)
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + path
}
