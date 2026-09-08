package sources

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Sidecar emits the directory-to-repository mapping, which is unrecoverable later: agents record
// cwd but no remote, and a reaped session cannot be traced back. A bounded probe, not a parser: the
// field names live in config, and a miss means project = none, which is a legal value.
type Sidecar struct{}

func (*Sidecar) Name() string { return "sidecar" }

// ProjectRecord is one directory-to-repository mapping.
type ProjectRecord struct {
	At   string `json:"at"`
	Kind string `json:"kind"`

	// ProjectDir is the agent's OWN encoded project directory name, the only join key present in trajectory paths.
	ProjectDir string `json:"project_dir"`

	// CWD is the working directory the probe found, placeholder applied.
	CWD string `json:"cwd,omitempty"`

	// Remote is normalised to host/path, or empty. Empty is a legal outcome.
	Remote string `json:"remote,omitempty"`

	// Project is the repository name, or absent. `project = none` is legal: not every agent is tied to a repository.
	Project string `json:"project,omitempty"`

	SourceID string `json:"source_id,omitempty"`

	// GaveUp records why no remote was found, so a gap is explained rather than merely empty.
	GaveUp string `json:"gave_up,omitempty"`
}

func (p *Sidecar) Discover(req Request) (Discovery, error) {
	src := req.Source
	d := Discovery{Health: AgentAbsent, Sniff: SniffOK}

	probe := src.CWDProbe
	if probe == nil {
		d.Reason = "sidecar source has no cwd_probe"
		return d, nil
	}

	// Inputs are the candidate files of the sources this probe names.
	inputs := map[string][]Candidate{}
	for _, other := range req.All {
		if !slices.Contains(probe.From, other.ID) || other.Root == "" || !other.Enabled {
			continue
		}
		// The map exists to name repositories; not the ones nobody wants named.
		found, _, _, _ := walkGlobs(other, req.Deny, req.Ignore)
		if len(found) > 0 {
			inputs[other.ID] = found
		}
	}
	if len(inputs) == 0 {
		d.Reason = "no probe input sources resolved"
		return d, nil
	}

	now := time.Now().UTC()
	if req.Now != nil {
		now = req.Now()
	}

	// One record per project directory, not per file: the mapping is a property of the directory.
	seen := map[string]bool{}
	var records []ProjectRecord

	for sourceID, candidates := range inputs {
		for _, c := range candidates {
			projectDir := projectDirOf(c.RelPath)
			if projectDir == "" || seen[projectDir] {
				continue
			}
			seen[projectDir] = true

			rec := ProjectRecord{
				At:   now.Format(time.RFC3339),
				Kind: "git_project_map",
				// Placeholdered at build time: this directory NAME encodes the username, and the file sits on disk between runs.
				ProjectDir: formats.ApplyUserPlaceholder(projectDir, req.Username),
				SourceID:   sourceID,
			}

			cwd, ok := probeCWD(c.Path, probe)
			if !ok {
				rec.GaveUp = "no cwd field found in the head of the file"
				records = append(records, rec)
				continue
			}
			rec.CWD = formats.ApplyUserPlaceholder(cwd, req.Username)

			remote, project, gaveUp := gitRemoteFor(cwd, src.GitRead)
			rec.Remote = remote
			rec.Project = project
			rec.GaveUp = gaveUp
			records = append(records, rec)
		}
	}

	slices.SortFunc(records, func(a, b ProjectRecord) int { return strings.Compare(a.ProjectDir, b.ProjectDir) })

	body, err := encodeJSONL(records, "project")
	if err != nil {
		return d, err
	}

	inventory, err := writeInventory(req.StateDir, src.ID, body)
	if err != nil {
		return d, err
	}

	d.Health = Collected
	d.Candidates = []Candidate{inventory}
	d.Reason = fmt.Sprintf("%d project directories mapped", len(records))
	return d, nil
}

// projectDirOf takes the agent's encoded project directory out of a relative path. Only a real
// projects/<encoded-cwd> segment counts: a bogus join key is worse than no record at all.
func projectDirOf(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, seg := range parts {
		if seg == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// probeCWD reads the first cwd-shaped value out of the head of a file, bounded on purpose: the client does not parse.
func probeCWD(path string, probe *CWDProbe) (string, bool) {
	budget := probe.ScanBytes
	if budget <= 0 {
		budget = 64 << 10
	}
	head, _, err := readHead(path, budget)
	if err != nil {
		return "", false
	}

	for line := range strings.SplitSeq(string(head), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		for _, field := range probe.Fields {
			if v, ok := lookupField(rec, field); ok && v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// lookupField resolves a dotted field name, so a config can name payload.cwd as easily as cwd.
func lookupField(rec map[string]json.RawMessage, field string) (string, bool) {
	parts := strings.Split(field, ".")
	current := rec
	for i, part := range parts {
		raw, ok := current[part]
		if !ok {
			return "", false
		}
		if i == len(parts)-1 {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", false
			}
			return s, true
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			return "", false
		}
		current = nested
	}
	return "", false
}

// gitRemoteFor walks up to the nearest .git and reads a url out of its config; insteadOf rules and credential helpers are ignored.
func gitRemoteFor(cwd string, cfg *GitRead) (remote, project, gaveUp string) {
	if cfg == nil {
		return "", "", "no git_read configured"
	}

	_, gitDir, ok := findGitDir(cwd, cfg, "")
	if !ok {
		// The trajectory outlives the checkout: a cwd that no longer exists is expected, not a failure.
		return "", "", "no .git found above " + filepath.Base(cwd)
	}

	raw, _, err := platform.ReadWhole(filepath.Join(gitDir, "config"), 1<<20)
	if err != nil {
		return "", "", "git config unreadable"
	}

	for _, u := range remoteURLs(string(raw), cfg.Take) {
		normalised, name, err := NormaliseRemote(u)
		if err != nil {
			continue
		}
		return normalised, name, ""
	}
	return "", "", "no usable remote in git config"
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

// remoteURLs pulls url values out of a git config, ignoring everything else.
func remoteURLs(body string, take []string) []string {
	wantURL := len(take) == 0
	for _, t := range take {
		if strings.Contains(t, "url") {
			wantURL = true
		}
	}
	if !wantURL {
		return nil
	}

	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(key) != "url" {
			continue
		}
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

// NormaliseRemote converts a remote to a stable host/path label and the project name. Userinfo is stripped
// UNCONDITIONALLY: a remote can carry a live credential. Local filesystem remotes are rejected, and the
// result drops scheme, port and .git suffix so ssh and https collapse to one label.
func NormaliseRemote(raw string) (hostPath, project string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("empty remote")
	}

	// scp-like syntax: git@github.com:org/repo.git
	if !strings.Contains(trimmed, "://") {
		if before, after, found := strings.Cut(trimmed, ":"); found && !strings.HasPrefix(trimmed, "/") {
			host := before
			if _, h, ok := strings.Cut(before, "@"); ok {
				host = h
			}
			return finishRemote(host, after)
		}
		// A bare path is a local remote.
		return "", "", fmt.Errorf("local filesystem remote")
	}

	u, parseErr := url.Parse(trimmed)
	if parseErr != nil {
		return "", "", parseErr
	}
	switch u.Scheme {
	case "file", "":
		return "", "", fmt.Errorf("local filesystem remote")
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("remote has no host")
	}
	// u.Hostname() drops userinfo AND the port, so no credential survives and one repository keeps one label.
	return finishRemote(u.Hostname(), u.Path)
}

func finishRemote(host, path string) (string, string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", "", fmt.Errorf("remote has no host")
	}
	path = strings.Trim(strings.TrimSuffix(strings.TrimSpace(path), ".git"), "/")
	if path == "" {
		return "", "", fmt.Errorf("remote has no path")
	}
	project := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		project = path[idx+1:]
	}
	return host + "/" + path, project, nil
}

// encodeJSONL renders records one JSON object per line, the inventory wire shape.
func encodeJSONL[T any](records []T, what string) ([]byte, error) {
	var body []byte
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("gather: encode %s record: %w", what, err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	return body, nil
}

// writeInventory replaces the source's inventory file: it describes what exists NOW, and appending would grow an unbounded log.
func writeInventory(stateDir, sourceID string, body []byte) (Candidate, error) {
	dir := filepath.Join(stateDir, "inventories")
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return Candidate{}, err
	}
	path := filepath.Join(dir, sourceID+".inventory.jsonl")
	if err := platform.WriteAtomic(path, body, 0o600); err != nil {
		return Candidate{}, err
	}

	// Assigned and closed: a descriptor whose lifetime the garbage collector decides is not a lifetime.
	f, info, err := platform.Open(path)
	if err != nil {
		return Candidate{}, err
	}
	defer f.Close()

	return Candidate{
		Path:    path,
		RelPath: sourceID + ".inventory.jsonl",
		Size:    info.Size(),
		MTime:   info.ModTime().UTC(),
	}, nil
}
