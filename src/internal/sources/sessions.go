package sources

import (
	"context"
	"crypto/sha256"
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

func discoverSessions(req Request, reader sessionReader) (Discovery, error) {
	src := req.Source
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
		sessions, err := reader.List(ctx, db.Path)
		if err != nil {
			d.Unreadable++
			d.UnreadableExample = db.Path
			d.UnreadableReason = err.Error()
			continue
		}
		for _, session := range sessions {
			// Keep arbitrary IDs out of virtual paths; raw identifiers remain inside records.
			name := fmt.Sprintf("%x.jsonl", sha256.Sum256([]byte(session.ID)))
			cand := Candidate{Path: filepath.Join(db.Path, "sessions", name), RelPath: filepath.ToSlash(filepath.Join(db.RelPath, "sessions", name)), CWD: session.CWD, MTime: now, AlwaysLoad: true}
			if req.Ignore.Match(src, cand) {
				d.Ignored = true
				continue
			}
			path, id := db.Path, session.ID
			cand.Load = func(ctx context.Context) (Payload, error) {
				// Recheck the path floor at load time as well as discovery.
				if info, err := os.Lstat(path); err != nil {
					return Payload{}, err
				} else if !info.Mode().IsRegular() {
					return Payload{}, fmt.Errorf("session database is not a regular file")
				}
				if req.Deny != nil {
					if denied, _ := req.Deny.Match(path); denied {
						return Payload{}, fmt.Errorf("session database denied")
					}
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
func sessionDatabases(src Resolved, deny *List) ([]Candidate, unreadable, error) {
	var out []Candidate
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
		path, named := filepath.Join(root, rel), filepath.Join(src.Root, rel)
		// Glob's literal prefix can follow symlinks even with WithNoFollow.
		nestedLink := false
		parent := root
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			parent = filepath.Join(parent, part)
			info, err := os.Lstat(parent)
			if err != nil {
				note(parent, err)
				nestedLink = true
				break
			}
			if info.Mode()&os.ModeSymlink != 0 {
				nestedLink = true
				break
			}
		}
		if nestedLink {
			continue
		}

		if deny != nil {
			if denied, _ := deny.MatchPair(named, path); denied {
				continue
			}
		}
		if matchesAny(filepath.ToSlash(rel), normalizeGlobs(src.Exclude)) {
			continue
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			note(path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, Candidate{Path: named, RelPath: filepath.ToSlash(rel)})
	}
	return out, bad, nil
}
