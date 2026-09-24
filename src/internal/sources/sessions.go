package sources

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// Session databases produce one bounded JSONL snapshot per session, never a database upload.
type sessionPrimitive struct{}

func (sessionPrimitive) Discover(req Request) (Discovery, error) {
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
		sessions, err := sqliteread.ListSessions(ctx, db.Path, src.Family)
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
				raw, err := sqliteread.ReadSession(ctx, path, src.Family, id, src.MaxFileBytes)
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

// Only fixed database locations are inspected; Hermes dependency and model caches can be enormous.
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
		if denied, _ := deny.MatchPair(src.Root, root); denied {
			return out, bad, nil
		}
	}
	names := []string{}
	switch src.Family {
	case "opencode":
		names = append(names, "opencode.db")
	case "hermes":
		names = append(names, "state.db")
		profiles := filepath.Join(root, "profiles")
		if info, err := os.Lstat(profiles); err == nil && info.IsDir() {
			entries, err := os.ReadDir(profiles)
			if err != nil {
				note(profiles, err)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					names = append(names, filepath.Join("profiles", entry.Name(), "state.db"))
				}
			}
		} else if err != nil && !os.IsNotExist(err) {
			note(profiles, err)
		}
	default:
		return nil, bad, fmt.Errorf("unsupported session family %q", src.Family)
	}
	for _, rel := range names {
		path, named := filepath.Join(root, rel), filepath.Join(src.Root, rel)
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
