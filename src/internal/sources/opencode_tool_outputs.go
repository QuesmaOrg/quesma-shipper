package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

type opencodeToolOutputs struct{}

var opencodeSpillName = regexp.MustCompile(`^tool_[A-Za-z0-9]+$`)

func (opencodeToolOutputs) Discover(req Request) (Discovery, error) {
	src := req.Source
	d := Discovery{Health: AgentAbsent, Sniff: SniffOK}
	if src.Root == "" {
		d.Reason = src.RootUnresolvedReason
		return d, nil
	}
	d.Health, d.Reason = RootPresentNoMatch, "no referenced tool outputs in enabled OpenCode transcripts"
	if src.MaxFileBytes <= 0 {
		return d, fmt.Errorf("OpenCode tool outputs require max_file_bytes")
	}
	root, err := filepath.EvalSymlinks(src.Root)
	if err != nil {
		return d, err
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return d, err
	}
	note := func(path string, err error) {
		d.Unreadable++
		d.UnreadableExample, d.UnreadableReason = path, err.Error()
	}
	seen := map[string]bool{}
	ignored := map[string]bool{}
	var referenced []Candidate
	for _, transcripts := range req.All {
		if transcripts.ID != "opencode-sessions" || !transcripts.Enabled || transcripts.Root != src.Root {
			continue
		}
		r := req
		r.Source = transcripts
		// Ignored sessions still veto shared spills; none of their transcript bytes are emitted here.
		r.Ignore = nil
		sessions, err := discoverSessions(r, sqliteread.OpenCodeSessions{})
		if err != nil {
			return d, err
		}
		d.Unreadable += sessions.Unreadable
		if sessions.Unreadable > 0 {
			d.UnreadableExample, d.UnreadableReason = sessions.UnreadableExample, sessions.UnreadableReason
		}
		d.Ignored = d.Ignored || sessions.Ignored
		for _, session := range sessions.Candidates {
			if err := ctx.Err(); err != nil {
				return d, err
			}
			ignoredSession := req.Ignore.Match(transcripts, session)
			d.Ignored = d.Ignored || ignoredSession
			payload, err := session.Load(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return d, ctx.Err()
				}
				note(session.Path, err)
				continue
			}
			for _, ref := range opencodeOutputPaths(payload.Bytes) {
				if err := ctx.Err(); err != nil {
					return d, err
				}
				name := filepath.Base(ref)
				// Only the canonical, flat spill namespace is addressable, never an arbitrary transcript path.
				rel := "tool-output/" + name
				if !opencodeSpillName.MatchString(name) || (ref != filepath.Join(src.Root, filepath.FromSlash(rel)) && ref != filepath.Join(root, filepath.FromSlash(rel))) {
					continue
				}
				if !matchesAny(rel, normalizeGlobs(src.Include)) || matchesAny(rel, normalizeGlobs(src.Exclude)) {
					continue
				}
				file := sessionDatabase{Path: filepath.Join(src.Root, filepath.FromSlash(rel)), RelPath: rel, root: root}
				if _, err := file.checkedPath(req.Deny); err != nil {
					if !os.IsNotExist(err) {
						note(file.Path, err)
					}
					continue
				}
				c := Candidate{Path: file.Path, RelPath: rel, CWD: session.CWD}
				if ignoredSession || req.Ignore.Match(src, c) {
					d.Ignored = true
					ignored[rel] = true
					continue
				}
				if !seen[rel] {
					seen[rel] = true
					referenced = append(referenced, c)
				}
			}
		}
	}
	for _, c := range referenced {
		if err := ctx.Err(); err != nil {
			return d, err
		}
		if ignored[c.RelPath] {
			continue
		}
		rel := c.RelPath
		file := sessionDatabase{Path: c.Path, RelPath: rel, root: root}
		path, err := file.checkedPath(req.Deny)
		if err != nil {
			if !os.IsNotExist(err) {
				note(file.Path, err)
			}
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			note(file.Path, err)
			continue
		}
		if info.Size() > src.MaxFileBytes {
			d.Oversize = append(d.Oversize, Oversize{RelPath: rel, Size: info.Size(), Limit: src.MaxFileBytes})
			continue
		}
		c.Size, c.MTime = info.Size(), info.ModTime().UTC()
		c.Load = func(ctx context.Context) (Payload, error) {
			if err := ctx.Err(); err != nil {
				return Payload{}, err
			}
			path, err := file.checkedPath(req.Deny)
			if err != nil {
				return Payload{}, err
			}
			before, err := os.Lstat(path)
			if err != nil {
				return Payload{}, err
			}
			raw, opened, err := platform.ReadWhole(path, src.MaxFileBytes)
			if err != nil {
				return Payload{}, err
			}
			if _, err := file.checkedPath(req.Deny); err != nil {
				return Payload{}, err
			}
			if !os.SameFile(before, opened) {
				return Payload{}, fmt.Errorf("tool output changed during open")
			}
			if err := ctx.Err(); err != nil {
				return Payload{}, err
			}
			return Payload{Bytes: raw, MTime: opened.ModTime().UTC()}, nil
		}
		d.Candidates = append(d.Candidates, c)
	}
	slices.SortFunc(d.Candidates, oldestFirst)
	switch {
	case len(d.Candidates) > 0:
		d.Health, d.Reason = Collected, ""
	case d.Unreadable > 0:
		d.Health, d.Reason = MatchPresentUnreadable, d.UnreadableReason
	case len(d.Oversize) > 0:
		d.Health, d.Reason = MatchPresentUnreadable, "referenced tool outputs exceed max_file_bytes"
	case d.Ignored:
		d.Reason = ignoredReason
	}
	return d, nil
}

// Read only typed tool metadata; text previews, user inputs and arbitrary metadata cannot name files.
func opencodeOutputPaths(raw []byte) []string {
	type tool struct {
		Type  string `json:"type"`
		State struct {
			Metadata struct {
				Truncated  bool   `json:"truncated"`
				OutputPath string `json:"outputPath"`
			} `json:"metadata"`
		} `json:"state"`
	}
	var paths []string
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var rec struct {
			Kind        string `json:"type"`
			MessageType string `json:"message_type"`
			Data        struct {
				tool
				Content []tool `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		tools := rec.Data.Content
		if rec.Kind == "part" {
			tools = []tool{rec.Data.tool}
		} else if rec.Kind != "session_message" || rec.MessageType != "assistant" {
			continue
		}
		for _, t := range tools {
			if t.Type == "tool" && t.State.Metadata.Truncated && t.State.Metadata.OutputPath != "" {
				paths = append(paths, t.State.Metadata.OutputPath)
			}
		}
	}
	return paths
}
