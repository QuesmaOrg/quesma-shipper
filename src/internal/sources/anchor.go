package sources

// Most Copilot files name no working directory, so each takes its folders from one anchor file
// in its session (CLI) or workspace-storage (VS Code) directory. A multi-root workspace answers
// with every local folder, and a .notrajectories marker in any of them drops the file.

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

type sessionAnchor struct {
	depth int // leading RelPath segments naming the directory
	file  string
	// read returns the anchor's folders, and whether that answer can no longer change and so
	// may be cached, misses included.
	read func(path string) (dirs []string, final bool)
}

func copilotAnchors(probe *CWDProbe) map[string]sessionAnchor {
	events := sessionAnchor{depth: 2, file: "events.jsonl", read: sessionStartCWD(probe)}
	workspace := sessionAnchor{depth: 1, file: "workspace.json", read: workspaceFolders}
	return map[string]sessionAnchor{
		"copilot-cli-sessions":         events,
		"copilot-cli-context":          events,
		"copilot-vscode-transcripts":   workspace,
		"copilot-vscode-chat-sessions": workspace,
		"copilot-vscode-editing":       workspace,
	}
}

func (f *RepoFilter) anchoredDirs(src Resolved, c Candidate, a sessionAnchor) []string {
	parts := strings.Split(filepath.ToSlash(c.RelPath), "/")
	if len(parts) <= a.depth || slices.Contains(parts[:a.depth], "..") {
		return nil
	}
	dir := filepath.Join(src.Root, filepath.Join(parts[:a.depth]...))
	key := src.Root + "\x00" + dir
	if dirs, hit := f.anchored[key]; hit {
		return dirs
	}
	found, final := a.read(filepath.Join(dir, a.file))
	var dirs []string
	for _, d := range found {
		if d = cleanCWD(d); d != "" {
			dirs = append(dirs, d)
		}
	}
	if final {
		f.anchored[key] = dirs
	}
	return dirs
}

// sessionStartCWD reads the CLI's first event, session.start, which is written whole before any
// other: once that line is complete, a missing cwd stays missing.
func sessionStartCWD(probe *CWDProbe) func(string) ([]string, bool) {
	return func(path string) ([]string, bool) {
		budget := probe.ScanBytes
		if budget <= 0 {
			budget = 64 << 10
		}
		head, _, err := readHead(path, budget)
		if err != nil {
			return nil, false
		}
		line, complete := firstLine(head)
		if !complete {
			return nil, false
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &rec) != nil {
			return nil, true
		}
		for _, field := range probe.Fields {
			if v, ok := lookupField(rec, field); ok && v != "" {
				return []string{v}, true
			}
		}
		return nil, true
	}
}

// workspaceFolders reads VS Code's workspace.json, written once when the workspace is first
// opened: one folder, or a .code-workspace file whose folders all count. A folder on another
// machine (vscode-remote) cannot carry a local marker, so it names nothing.
func workspaceFolders(path string) ([]string, bool) {
	body, _, err := platform.ReadWhole(path, 64<<10)
	if err != nil {
		return nil, false
	}
	var ws struct {
		Folder    string `json:"folder"`
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(body, &ws) != nil {
		return nil, false
	}
	if ws.Folder != "" {
		if p, ok := localPath(ws.Folder); ok {
			return []string{p}, true
		}
		return nil, true
	}
	file, ok := localPath(ws.Workspace)
	if !ok {
		return nil, true
	}
	return codeWorkspaceFolders(file)
}

// codeWorkspaceFolders lists a .code-workspace file's folders: a path relative to the file or
// absolute, or a URI. Folders added later count from the next plan, as the answer is cached.
func codeWorkspaceFolders(file string) ([]string, bool) {
	body, _, err := platform.ReadWhole(file, 1<<20)
	if err != nil {
		return nil, false
	}
	var doc struct {
		Folders []struct {
			Path string `json:"path"`
			URI  string `json:"uri"`
		} `json:"folders"`
	}
	if json.Unmarshal(stripJSONC(body), &doc) != nil {
		return nil, false
	}
	var dirs []string
	for _, folder := range doc.Folders {
		switch {
		case folder.URI != "":
			if p, ok := localPath(folder.URI); ok {
				dirs = append(dirs, p)
			}
		case folder.Path != "":
			p := filepath.FromSlash(folder.Path)
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(file), p)
			}
			dirs = append(dirs, p)
		}
	}
	return dirs, true
}

// localPath turns a file URI into a local path. A host is a UNC share or \\wsl.localhost, which
// only Windows can open.
func localPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", false
	}
	p := u.Path
	// file:///c%3A/Users/... decodes to /c:/Users/...; the drive letter needs no leading slash.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	if u.Host != "" && u.Host != "localhost" {
		if runtime.GOOS != "windows" {
			return "", false
		}
		p = "//" + u.Host + p
	}
	return p, true
}

// stripJSONC removes the comments and trailing commas VS Code allows in a .code-workspace file.
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString, escaped := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			i += end + 3
		case c == '}' || c == ']':
			trimmed := bytes.TrimRight(out, " \t\r\n")
			if len(trimmed) > 0 && trimmed[len(trimmed)-1] == ',' {
				out = trimmed[:len(trimmed)-1]
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}
