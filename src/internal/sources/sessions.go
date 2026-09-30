package sources

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// Session databases produce one bounded JSONL snapshot per session, never a database upload.
type sessionReader interface {
	List(context.Context, string) ([]sqliteread.Session, error)
	Read(context.Context, string, string, int64) ([]byte, error)
}

func sessionCollector(emit string) (Primitive, bool) {
	switch emit {
	case "opencode_sessions":
		return opencodeSessions{}, true
	case "hermes_sessions":
		return hermesSessions{}, true
	default:
		return nil, false
	}
}

// Keep the existing manifest gather name; session-specific collectors own the reads.
type sessionDispatch struct{}

func (sessionDispatch) Discover(req Request) (Discovery, error) {
	collector, ok := sessionCollector(req.Source.Emit)
	if !ok {
		return Discovery{}, fmt.Errorf("unknown session collector %q", req.Source.Emit)
	}
	return collector.Discover(req)
}

func discoverSessions(req Request, reader sessionReader) (Discovery, error) {
	src := req.Source
	if src.MaxFileBytes <= 0 {
		src.MaxFileBytes = sqliteread.DefaultSessionMaxBytes
	}
	d := Discovery{Health: AgentAbsent, Sniff: SniffOK}
	if src.Root == "" {
		d.Reason = src.RootUnresolvedReason
		return d, nil
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	dbs, bad, err := sessionDatabases(src, req.Deny)
	if err != nil {
		return d, err
	}
	d.Unreadable, d.UnreadableExample, d.UnreadableReason = bad.count, bad.example, bad.reason()
	now := time.Now().UTC()
	if req.Now != nil {
		now = req.Now()
	}
	for _, db := range dbs {
		path, err := db.checkedPath(req.Deny)
		var sessions []sqliteread.Session
		if err == nil {
			sessions, err = reader.List(ctx, path)
		}
		if err != nil {
			d.Unreadable++
			d.UnreadableExample = db.Path
			d.UnreadableReason = err.Error()
			continue
		}
		for _, session := range sessions {
			// Keep arbitrary IDs out of virtual paths; raw identifiers remain inside records.
			name := fmt.Sprintf("%x.jsonl", sha256.Sum256([]byte(session.ID)))
			cand := Candidate{Path: filepath.Join(db.Path, "sessions", name), RelPath: filepath.ToSlash(filepath.Join(db.RelPath, "sessions", name)), CWD: session.CWD, Size: src.MaxFileBytes, MTime: now, AlwaysLoad: true}
			if req.Ignore.Match(src, cand) {
				d.Ignored = true
				continue
			}
			id := session.ID
			cand.Load = func(ctx context.Context) (Payload, error) {
				path, err := db.checkedPath(req.Deny)
				if err != nil {
					return Payload{}, err
				}
				raw, err := reader.Read(ctx, path, id, src.MaxFileBytes)
				return Payload{Bytes: raw, MTime: now}, err
			}
			d.Candidates = append(d.Candidates, cand)
		}
	}
	switch {
	case len(d.Candidates) > 0:
		d.Health = Collected
	case d.Unreadable > 0:
		d.Health = MatchPresentUnreadable
		d.Reason = d.UnreadableReason
	case d.Ignored:
		d.Health = RootPresentNoMatch
		d.Reason = ignoredReason
	default:
		d.Health = RootPresentNoMatch
		d.Reason = "no sessions found in the configured databases"
	}
	return d, nil
}

// Expand declared patterns without walking unrelated dependency and model caches.
func sessionDatabases(src Resolved, deny *List) ([]sessionDatabase, unreadable, error) {
	var out []sessionDatabase
	var bad unreadable
	note := func(path string, err error) {
		bad.count++
		if bad.err == nil {
			bad.example, bad.err = path, err
		}
	}
	root, err := filepath.EvalSymlinks(src.Root)
	if err != nil {
		note(src.Root, err)
		return out, bad, nil
	}
	if deny != nil {
		if denied, pattern := deny.MatchPair(src.Root, root); denied {
			note(src.Root, fmt.Errorf("root resolves to %s, which the deny list refuses (%s)", root, pattern))
			return out, bad, nil
		}
	}
	names := []string{}
	seen := map[string]bool{}
	for _, pattern := range normalizeGlobs(src.Include) {
		matches, err := doublestar.Glob(os.DirFS(root), pattern, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
		if err != nil {
			note(root, err)
			continue
		}
		for _, name := range matches {
			if !seen[name] {
				names = append(names, name)
				seen[name] = true
			}
		}
	}
	for _, rel := range names {
		if matchesAny(filepath.ToSlash(rel), normalizeGlobs(src.Exclude)) {
			continue
		}
		db := sessionDatabase{Path: filepath.Join(src.Root, rel), RelPath: filepath.ToSlash(rel), root: root}
		if _, err := db.checkedPath(deny); err != nil {
			if !errors.Is(err, errSessionPathRejected) && !os.IsNotExist(err) {
				note(db.Path, err)
			}
			continue
		}
		out = append(out, db)
	}
	return out, bad, nil
}

var errSessionPathRejected = errors.New("session database path rejected")

type sessionDatabase struct {
	Path, RelPath string
	root          string
}

// SQLite opens by name, so this rechecks scope before each read but cannot make validation and opening atomic.
func (db sessionDatabase) checkedPath(deny *List) (string, error) {
	root, err := filepath.EvalSymlinks(db.root)
	if err != nil {
		return "", err
	}
	if root != db.root || !filepath.IsLocal(filepath.FromSlash(db.RelPath)) {
		return "", fmt.Errorf("%w: resolved root changed or path is not local", errSessionPathRejected)
	}
	path := root
	for _, part := range strings.Split(db.RelPath, "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: symlink at %s", errSessionPathRejected, path)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: not a regular file", errSessionPathRejected)
	}
	if deny != nil {
		if denied, pattern := deny.MatchPair(db.Path, path); denied {
			return "", fmt.Errorf("%w: deny list refuses %s (%s)", errSessionPathRejected, path, pattern)
		}
	}
	return path, nil
}
