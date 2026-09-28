package sources

import (
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
)

func copilotFilter(home string) *RepoFilter {
	probe := &CWDProbe{Fields: []string{"data.context.cwd"}, ScanBytes: 64 << 10}
	f := newRepoFilter(probe, nil, home)
	f.anchors = copilotAnchors(probe)
	return f
}

func copilotStart(cwd string) string {
	return `{"type":"session.start","data":{"sessionId":"s","context":{"cwd":` + strconv.Quote(cwd) + `}}}` + "\n"
}

func folderJSON(dir string) string {
	return `{"folder":` + strconv.Quote("file://"+filepath.ToSlash(slashed(dir))) + `}`
}

// slashed gives a Windows path the leading slash a file URI needs before its drive letter.
func slashed(p string) string {
	if filepath.VolumeName(p) != "" {
		return "/" + p
	}
	return p
}

// Sidecars carry no cwd of their own, so they follow their session's events.jsonl; a session
// whose events.jsonl is not written yet is unattributed until it is.
func TestCopilotSidecarsFollowTheirSession(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	keeper := repoDir(t, home, "work/keeper", false)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "session-state/s1/events.jsonl"), copilotStart(acme))
	writeFile(t, filepath.Join(root, "session-state/s2/events.jsonl"), copilotStart(keeper))
	f := copilotFilter(home)
	cli := Resolved{Source: Source{ID: "copilot-cli-sessions"}, Root: root}
	ctx := Resolved{Source: Source{ID: "copilot-cli-context"}, Root: root}

	for _, tc := range []struct {
		src  Resolved
		rel  string
		want bool
	}{
		{cli, "session-state/s1/events.jsonl", true},
		{ctx, "session-state/s1/workspace.yaml", true},
		{ctx, "session-state/s1/rewind-file-snapshots/3f9a0c1e", true},
		{ctx, "session-state/s2/plan.md", false},
		{ctx, "session-state/s3/plan.md", false},
	} {
		if got := f.Match(tc.src, candidateFor(root, tc.rel)); got != tc.want {
			t.Errorf("Match(%s) = %v, want %v", tc.rel, got, tc.want)
		}
	}

	writeFile(t, filepath.Join(root, "session-state/s3/events.jsonl"), copilotStart(acme))
	if !f.Match(ctx, candidateFor(root, "session-state/s3/plan.md")) {
		t.Error("a sidecar seen before its events.jsonl stayed unattributed after the session started")
	}
}

// A miss is cached only once it is final: a complete session.start line without a cwd stays
// without one, while a first line still being written may yet name it.
func TestAnAnchorMissIsCachedOnlyWhenFinal(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	root := t.TempDir()
	ctx := Resolved{Source: Source{ID: "copilot-cli-context"}, Root: root}
	f := copilotFilter(home)

	torn := filepath.Join(root, "session-state/torn/events.jsonl")
	writeFile(t, torn, `{"type":"session.start","data":{"context":`)
	if f.Match(ctx, candidateFor(root, "session-state/torn/plan.md")) {
		t.Fatal("a torn first line matched")
	}
	writeFile(t, torn, copilotStart(acme))
	if !f.Match(ctx, candidateFor(root, "session-state/torn/plan.md")) {
		t.Error("a torn first line was cached as a miss")
	}

	final := filepath.Join(root, "session-state/final/events.jsonl")
	writeFile(t, final, `{"type":"session.start","data":{"sessionId":"s"}}`+"\n")
	if f.Match(ctx, candidateFor(root, "session-state/final/plan.md")) {
		t.Fatal("a session.start without a cwd matched")
	}
	writeFile(t, final, copilotStart(acme))
	if f.Match(ctx, candidateFor(root, "session-state/final/plan.md")) {
		t.Error("a final miss was not cached: the anchor was read again")
	}
}

func TestVSCodeChatsFollowTheirWorkspaceFolders(t *testing.T) {
	home := t.TempDir()
	acme := repoDir(t, home, "work/acme", true)
	repoDir(t, home, "work/keeper", false) // named relatively by both .code-workspace files
	other := repoDir(t, home, "work/other", false)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "h1/workspace.json"), folderJSON(acme))
	writeFile(t, filepath.Join(root, "h2/workspace.json"), `{"folder":"vscode-remote://ssh-remote%2Bbox/home/jane/acme"}`)

	// Multi-root: VS Code writes comments and trailing commas, and relative paths resolve
	// against the .code-workspace file.
	marked := filepath.Join(home, "work", "marked.code-workspace")
	writeFile(t, marked, `{
  // the api and its client
  "folders": [
    {"path": "keeper"},
    {"uri": `+strconv.Quote("file://"+filepath.ToSlash(slashed(acme)))+`}, /* opted out */
  ],
  "settings": {},
}`)
	writeFile(t, filepath.Join(root, "h3/workspace.json"), `{"workspace":`+strconv.Quote("file://"+filepath.ToSlash(slashed(marked)))+`}`)
	clean := filepath.Join(home, "work", "clean.code-workspace")
	writeFile(t, clean, `{"folders":[{"path":"keeper"},{"path":`+strconv.Quote(other)+`},{"uri":"vscode-remote://wsl%2Bubuntu/home/jane/x"}]}`)
	writeFile(t, filepath.Join(root, "h4/workspace.json"), `{"workspace":`+strconv.Quote("file://"+filepath.ToSlash(slashed(clean)))+`}`)
	f := copilotFilter(home)

	for _, tc := range []struct {
		id, rel string
		want    bool
	}{
		{"copilot-vscode-transcripts", "h1/GitHub.copilot-chat/transcripts/s1.jsonl", true},
		{"copilot-vscode-chat-sessions", "h1/chatSessions/s1.jsonl", true},
		{"copilot-vscode-editing", "h1/chatEditingSessions/s1/contents/a1b2", true},
		{"copilot-vscode-chat-sessions", "h2/chatSessions/s1.jsonl", false},
		{"copilot-vscode-chat-sessions", "h3/chatSessions/s1.jsonl", true},
		{"copilot-vscode-chat-sessions", "h4/chatSessions/s1.jsonl", false},
		{"copilot-vscode-chat-sessions", "h5/chatSessions/s1.jsonl", false},
		{"copilot-vscode-global-sessions", "emptyWindowChatSessions/s1.jsonl", false},
	} {
		src := Resolved{Source: Source{ID: tc.id}, Root: root}
		if got := f.Match(src, candidateFor(root, tc.rel)); got != tc.want {
			t.Errorf("Match(%s %s) = %v, want %v", tc.id, tc.rel, got, tc.want)
		}
	}

	src := Resolved{Source: Source{ID: "copilot-vscode-chat-sessions"}, Root: root}
	if got := f.CWD(src, candidateFor(root, "h1/chatSessions/s1.jsonl")); got != acme {
		t.Errorf("single-folder CWD = %q, want %q", got, acme)
	}
	if got := f.CWD(src, candidateFor(root, "h4/chatSessions/s1.jsonl")); got != "" {
		t.Errorf("multi-root CWD = %q, want none", got)
	}
}

func TestLocalPathReadsOnlyAPathThisMachineCanOpen(t *testing.T) {
	unc := "//nas/code/repo"
	if runtime.GOOS != "windows" {
		unc = ""
	}
	for _, tc := range []struct{ uri, want string }{
		{"file:///Users/jane/work/my%20app", "/Users/jane/work/my app"},
		{"file:///c%3A/Users/jane/api", "c:/Users/jane/api"},
		{"file://localhost/Users/jane/api", "/Users/jane/api"},
		{"file://nas/code/repo", unc},
		{"vscode-remote://ssh-remote%2Bbox/home/jane/api", ""},
		{"", ""},
	} {
		got, ok := localPath(tc.uri)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("localPath(%q) = %q, %v; want %q", tc.uri, got, ok, tc.want)
		}
	}
}

func TestWorkspaceFoldersIsFinalOnlyForAWholeDocument(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		body  string
		dirs  []string
		final bool
	}{
		{`{"folder":"file:///work/api"}`, []string{"/work/api"}, true},
		{`{"folder":"vscode-remote://ssh-remote%2Bbox/api"}`, nil, true},
		{`{"workspace":"file:///nowhere/x.code-workspace"}`, nil, false},
		{`{}`, nil, true},
		{`{"folder":`, nil, false},
	} {
		p := filepath.Join(dir, "workspace.json")
		writeFile(t, p, tc.body)
		dirs, final := workspaceFolders(p)
		if !slices.Equal(dirs, tc.dirs) || final != tc.final {
			t.Errorf("workspaceFolders(%s) = %q, %v; want %q, %v", tc.body, dirs, final, tc.dirs, tc.final)
		}
	}
}

func TestStripJSONCKeepsStrings(t *testing.T) {
	in := `{"a": "http://x/*y*/", // c
  "b": [1, 2,], /* d */ "c": "\"//\"",}`
	if got, want := string(stripJSONC([]byte(in))), `{"a": "http://x/*y*/", `+"\n"+`  "b": [1, 2],  "c": "\"//\""}`; got != want {
		t.Errorf("stripJSONC\n got %s\nwant %s", got, want)
	}
}
