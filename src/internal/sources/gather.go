// Package sources discovers what there is to collect. It never reads file contents: the
// engine owns reading, through platform. An undetected discovery failure is permanent loss,
// so Discover emits health even when it finds zero bytes.
package sources

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// HealthState is the per-source discovery verdict, aliased so gather and the heartbeat document agree on one set of strings.
type HealthState = formats.HealthState

const (
	AgentAbsent            = formats.AgentAbsent
	RootPresentNoMatch     = formats.RootPresentNoMatch
	MatchPresentUnreadable = formats.MatchPresentUnreadable
	Collected              = formats.Collected
)

type SniffResult = formats.SniffResult

const (
	SniffOK              = formats.SniffOK
	SniffEmpty           = formats.SniffEmpty
	SniffUnexpectedShape = formats.SniffUnexpectedShape
	SniffUnreadable      = formats.SniffUnreadable
)

// Candidate is one discovered file.
type Candidate struct {
	// Path is absolute, as opened.
	Path string

	// RelPath is relative to the resolved root and derives the mirror key, so a store that moves keeps its keys.
	RelPath string

	Size  int64
	MTime time.Time
}

// Discovery is what one source's discovery pass found, plus why.
type Discovery struct {
	Health HealthState

	// SniffFailures counts sampled files that were unreadable or the wrong shape; it is a source-wide verdict only when every sample failed.
	SniffFailures int

	// Oversize lists files the size cap excluded. Reported even on a healthy source: they never ship and the store's reaper deletes them.
	Oversize []Oversize

	// Unreadable counts what the walk could not look at. Reported even on a healthy source: a skipped subtree never ships again.
	Unreadable int

	// UnreadableExample is the first path that failed, and UnreadableReason its error.
	UnreadableExample string
	UnreadableReason  string

	// Sniff is the shape verdict for the sampled candidates. It lands verbatim in manifests, heartbeats and doctor output.
	Sniff SniffResult

	// AgentVersion is read from the store itself, making a downstream parse-failure spike attributable to an agent release.
	AgentVersion string

	// Candidates are oldest-first: for a source with a reaper, the files closest to deletion cannot be collected later.
	Candidates []Candidate

	// Reason explains a non-collected health state, for doctor and the heartbeat.
	Reason string

	// Ignored says the ignore list dropped something, so a source emptied by it is not
	// mistaken for drift. A bool: nothing reports on an ignored repository.
	Ignored bool
}

// Request is everything a discovery pass is given; each primitive uses a different slice of it.
type Request struct {
	Source Resolved

	// All is every resolved source, for a primitive whose input is another source's files.
	All []Resolved

	// Deny is install-wide: the compiled list plus served additions, not something a source configures.
	Deny *List

	// Ignore drops candidates of an ignored repository. Install-wide like Deny; nil
	// means nothing is ignored.
	Ignore *RepoFilter

	// StateDir is the ONLY place a primitive may write its derived artifacts, never inside an agent's store.
	StateDir string

	// Username feeds the path placeholder, so an inventory record carries a pseudonymised path.
	Username string

	Now func() time.Time
}

// Primitive discovers candidates for one source. Config can never introduce one: primitives are code, sources are data.
type Primitive interface {
	Name() string
	Discover(req Request) (Discovery, error)
}

// Registry is the compiled primitive set.
type Registry struct {
	primitives map[string]Primitive
}

// NewRegistry builds the registry. Reserved names (acp, cloud_pull) are absent by construction, so a source naming one fails validation.
func NewRegistry() *Registry {
	r := &Registry{primitives: map[string]Primitive{}}
	for _, p := range []Primitive{
		// file_glob: append-only JSONL stores, copied config files, spill directories.
		// compressed_file: already-compressed files by whole-file hash, advertised separately.
		globPrimitive{name: "file_glob"},
		globPrimitive{name: "compressed_file"},
		&Sidecar{},
	} {
		r.primitives[p.Name()] = p
	}
	return r
}

// For returns the primitive a source declares.
func (r *Registry) For(gather string) (Primitive, error) {
	p, ok := r.primitives[gather]
	if !ok {
		return nil, fmt.Errorf("gather: no compiled primitive %q", gather)
	}
	return p, nil
}

// globPrimitive walks a root and matches include globs; both glob-shaped gathers share it.
type globPrimitive struct{ name string }

func (p globPrimitive) Name() string { return p.name }

func (p globPrimitive) Discover(req Request) (Discovery, error) {
	return discoverByGlob(req)
}

func discoverByGlob(req Request) (Discovery, error) {
	src := req.Source
	d := Discovery{Health: AgentAbsent}

	if src.Root == "" {
		d.Reason = cmp.Or(src.RootUnresolvedReason, "no candidate root resolved")
		return d, nil
	}

	matched, oversize, bad, ignored := walkGlobs(src, req.Deny, req.Ignore)
	d.Oversize, d.Ignored = oversize, ignored
	d.Unreadable, d.UnreadableExample = bad.count, bad.example
	d.UnreadableReason = bad.reason()

	if len(matched) == 0 {
		// Root present, globs matched nothing: probable drift, and never to be confused with agent_absent.
		d.Health = RootPresentNoMatch
		d.Reason = fmt.Sprintf("root %s exists but no file matched %v", src.Root, src.Include)
		switch {
		case bad.count > 0:
			d.Health = MatchPresentUnreadable
			d.Reason = bad.reason()
		case d.Ignored:
			d.Reason = ignoredReason
		case len(oversize) > 0:
			// Every match was over the cap: saying "no file matched" would send the reader to their globs instead.
			d.Health = MatchPresentUnreadable
			d.Reason = fmt.Sprintf("%d file(s) matched but every one is over the %d-byte cap",
				len(oversize), src.MaxFileBytes)
		}
		return d, nil
	}

	// Oldest first: for a source with a known reaper these are the ones closest to deletion.
	slices.SortFunc(matched, func(a, b Candidate) int {
		if c := a.MTime.Compare(b.MTime); c != 0 {
			return c
		}
		return strings.Compare(a.RelPath, b.RelPath)
	})

	d.Candidates = matched
	d.Health = Collected
	// Sampled, not single-file: the source is condemned only if EVERY sample fails, since one bad file is the per-file path's problem.
	d.Sniff, d.AgentVersion, d.SniffFailures = sniffSample(matched, src.Sniff)
	if d.Sniff == SniffUnreadable || d.Sniff == SniffUnexpectedShape {
		// Found but unusable: a store that switched substrate lands here rather than shipping garbage.
		d.Health = MatchPresentUnreadable
		d.Reason = fmt.Sprintf("shape sniff failed on every one of %d sampled files; last was %s",
			d.SniffFailures, d.Sniff)
		d.Candidates = nil
	}
	return d, nil
}

// ignoredReason explains a source emptied by the ignore list: the config working, not drift.
const ignoredReason = "every file this source found belongs to a repository you are not tracking"

// unreadable is what a walk could not look at, counted rather than collapsed into a single error.
type unreadable struct {
	count   int
	example string
	err     error
}

func (u unreadable) reason() string {
	if u.count == 0 {
		return ""
	}
	if u.count == 1 {
		return fmt.Sprintf("%s: %v", u.example, u.err)
	}
	return fmt.Sprintf("%d paths unreadable, first %s: %v", u.count, u.example, u.err)
}

// walkGlobs walks the root once against the globs, the deny list and the ignore list
// (the last return says whether ignore dropped anything); symlinks below the root are
// never followed, which pairs with O_NOFOLLOW at open time.
func walkGlobs(src Resolved, deny *List, ignore *RepoFilter) ([]Candidate, []Oversize, unreadable, bool) {
	var out []Candidate
	var oversize []Oversize
	var bad unreadable
	include, exclude := normalizeGlobs(src.Include), normalizeGlobs(src.Exclude)
	ignored := false

	// Dir to its resolved form: a candidate is never a symlink, so resolving the parent answers for every file in it.
	resolvedDirs := map[string]string{}
	resolveDir := func(dir string) string {
		if r, ok := resolvedDirs[dir]; ok {
			return r
		}
		r, err := filepath.EvalSymlinks(dir)
		if err != nil {
			r = ""
		}
		resolvedDirs[dir] = r
		return r
	}

	// resolveName is one file's resolved spelling, or "" when the parent resolves to itself or not at all.
	resolveName := func(p string) string {
		dir := filepath.Dir(p)
		if dir == "" {
			return ""
		}
		rdir := resolveDir(dir)
		if rdir == "" || rdir == dir {
			return ""
		}
		return filepath.Join(rdir, filepath.Base(p))
	}

	note := func(path string, err error) {
		bad.count++
		if bad.err == nil {
			bad.example, bad.err = path, err
		}
	}

	// The ROOT may be a symlink, and only the root: ~/.claude -> ~/dotfiles/claude is what stow and chezmoi
	// produce, and WalkDir would lstat it and descend into nothing. Entries inside are still not followed, and
	// the resolved root is re-checked against the deny list.
	root := src.Root
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil && resolved != root {
		if deny != nil {
			if denied, pattern := deny.Match(resolved); denied {
				note(root, fmt.Errorf("root resolves to %s, which the deny list refuses (%s)",
					resolved, pattern))
				return out, oversize, bad, ignored
			}
		}
		root = resolved
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A permission denial deep in a store must not abort the walk; what could not be read is counted.
			note(path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// MatchTree, not Match: only a whole-tree pattern licenses pruning, and the per-file check below stays the authority.
			if deny != nil {
				if denied, _ := deny.MatchTree(path); denied {
					return fs.SkipDir
				}
			}
			return nil
		}
		// Type() reports the entry's own type without following it.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if !matchesAny(rel, include) || matchesAny(rel, exclude) {
			return nil
		}
		// The path as the operator spells it: what downstream records, and what the deny list is written against.
		named := filepath.Join(src.Root, rel)

		if deny != nil {
			// BOTH spellings: the walk may run under a resolved root (~/.claude -> ~/dotfiles/claude, /var -> /private/var),
			// so a rule written against one does not match a string built from the other. A file is denied if EITHER name is.
			if denied, _ := deny.MatchPair(path, resolveName(path)); denied {
				return nil
			}
			if named != path {
				if denied, _ := deny.MatchPair(named, resolveName(named)); denied {
					return nil
				}
			}
		}

		// Before the size cap, so an ignored repository's oversized file is not reported
		// by name either.
		if ignore.Match(src, Candidate{Path: named, RelPath: rel}) {
			ignored = true
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			note(path, infoErr)
			return nil
		}
		if src.MaxFileBytes > 0 && info.Size() > src.MaxFileBytes {
			// Over the budget: counted on its own channel, since a policy decision is not a path that could not be read.
			oversize = append(oversize, Oversize{
				RelPath: rel, Size: info.Size(), Limit: src.MaxFileBytes,
			})
			return nil
		}

		// Reported under the CONFIGURED root, not the resolved one: native_path, the audit log and the state key must
		// keep the operator's spelling, or resolving a symlink orphans every fingerprint in the archive.
		out = append(out, Candidate{
			Path:    named,
			RelPath: rel,
			Size:    info.Size(),
			MTime:   info.ModTime().UTC(),
		})
		return nil
	})
	// A walk that failed at the root: WalkDir's own error, which the callback never saw.
	if err != nil && bad.err == nil {
		bad.count++
		bad.example, bad.err = src.Root, err
	}
	return out, oversize, bad, ignored
}

// Oversize is one file the size cap kept out of a run.
type Oversize struct {
	RelPath string
	Size    int64
	Limit   int64
}

// matchesAny expects patterns already normalised by normalizeGlobs.
func matchesAny(rel string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := doublestar.Match(p, rel); err == nil && ok {
			return true
		}
	}
	return false
}

func normalizeGlobs(globs []string) []string {
	out := make([]string, len(globs))
	for i, g := range globs {
		out[i] = strings.TrimPrefix(filepath.ToSlash(g), "/")
	}
	return out
}

// sniff runs the catalog's shape assertion: shape, never semantics, so a drifted format is a data fix rather than a release.
func sniff(path string, spec *Sniff) (SniffResult, string) {
	if spec == nil || spec.Kind == "" || spec.Kind == "none" {
		return SniffOK, ""
	}

	budget := spec.MaxScanBytes
	if budget <= 0 {
		budget = 64 << 10
	}
	head, info, err := readHead(path, budget)
	if err != nil {
		return SniffUnreadable, ""
	}
	if len(head) == 0 {
		return SniffEmpty, ""
	}
	// Whether the head stopped at the budget or at end of file changes what a missing newline means.
	truncated := info != nil && info.Size() > int64(len(head))

	switch spec.Kind {
	case "jsonl":
		return sniffJSONL(head, truncated)
	case "json":
		if firstNonEmptyByte(head) != '{' && firstNonEmptyByte(head) != '[' {
			return SniffUnexpectedShape, ""
		}
		return SniffOK, ""
	case "magic":
		want, err := hex.DecodeString(spec.MagicHex)
		if err != nil || len(head) < len(want) {
			return SniffUnexpectedShape, ""
		}
		for i := range want {
			if head[i] != want[i] {
				return SniffUnexpectedShape, ""
			}
		}
		return SniffOK, ""
	case "text":
		if looksBinary(head) {
			return SniffUnexpectedShape, ""
		}
		return SniffOK, ""
	default:
		return SniffOK, ""
	}
}

func readHead(path string, budget int64) ([]byte, os.FileInfo, error) {
	f, info, err := platform.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	if info.Size() == 0 {
		return nil, info, nil
	}
	buf := make([]byte, min(budget, info.Size()))
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, info, err
	}
	return buf[:n], info, nil
}

func firstNonEmptyByte(b []byte) byte {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

func looksBinary(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// sniffSampleSize is how many files are asked before condemning a source, spread across the ordering rather than taken from one end.
const sniffSampleSize = 5

// sniffSample asks several files and returns the best answer. Deterministic: a source must not oscillate across ticks.
func sniffSample(matched []Candidate, spec *Sniff) (SniffResult, string, int) {
	if len(matched) == 0 {
		return SniffUnreadable, "", 0
	}
	idx := sampleIndexes(len(matched), sniffSampleSize)

	var best, okResult SniffResult
	failures, ok, version := 0, false, ""
	// Scan the whole sample so every unreadable file is counted, and take the version from the
	// newest readable one (idx ascends by mtime): the closest proxy for the current install.
	for i, at := range idx {
		result, agentVersion := sniff(matched[at].Path, spec)
		if result == SniffUnreadable || result == SniffUnexpectedShape {
			failures++
			if i == 0 {
				best = result
			}
			continue
		}
		ok, okResult, version = true, result, agentVersion
	}
	if !ok {
		return best, "", failures
	}
	return okResult, version, failures
}

// sampleIndexes picks up to n positions spread evenly across length, always including the first and the last.
func sampleIndexes(length, n int) []int {
	if length <= n {
		out := make([]int, length)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, i*(length-1)/(n-1))
	}
	return out
}
