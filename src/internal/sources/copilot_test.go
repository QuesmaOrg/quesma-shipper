package sources_test

// The Copilot roots hold credentials beside the sessions, so these pin exactly what the catalog
// collects from a store laid out like a real CLI 1.0.88 and VS Code Copilot Chat install.

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func collected(t *testing.T, id, root string, deny *sources.List) []string {
	t.Helper()
	c, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.Source(id)
	if !ok {
		t.Fatalf("%s is not in the catalog", id)
	}
	d := discover(t, config.ResolvedSource{Source: s, Root: root, Enabled: true}, deny)
	var got []string
	for _, cand := range d.Candidates {
		got = append(got, filepath.ToSlash(cand.RelPath))
	}
	slices.Sort(got)
	return got
}

func TestCopilotCLIRootYieldsOnlySessionFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".copilot")
	for _, rel := range []string{
		"session-state/s1/events.jsonl",
		"session-state/s1/workspace.yaml",
		"session-state/s1/plan.md",
		"session-state/s1/checkpoints/index.md",
		"session-state/s1/research/keys.md",
		"session-state/s1/files/notes.txt",
		"session-state/s1/rewind-file-snapshots/tracking.json",
		"session-state/s1/rewind-file-snapshots/logo.png",
		"session-state/s1/inuse.90068.lock",
		"session-state/s1/vscode.metadata.json",
		"session-state/.session-operation-locks/s1.lock",
		"config.json",
		"mcp-config.json",
		"mcp-oauth-config/github.json",
		"ide/20d3f075.lock",
		"command-history-state.json",
		"session-store.db",
		"logs/process-1.log",
	} {
		write(t, filepath.Join(root, rel), `{"a":1}`+"\n")
	}
	deny := sources.New(home)

	if got, want := collected(t, "copilot-cli-sessions", root, deny), []string{"session-state/s1/events.jsonl"}; !slices.Equal(got, want) {
		t.Errorf("sessions collected %v, want %v", got, want)
	}
	want := []string{
		"session-state/s1/checkpoints/index.md",
		"session-state/s1/files/notes.txt",
		"session-state/s1/plan.md",
		"session-state/s1/research/keys.md",
		"session-state/s1/rewind-file-snapshots/tracking.json",
		"session-state/s1/workspace.yaml",
	}
	if got := collected(t, "copilot-cli-context", root, deny); !slices.Equal(got, want) {
		t.Errorf("context collected %v, want %v", got, want)
	}
}

func TestVSCodeWorkspaceStorageYieldsOnlyChatFiles(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"h1/workspace.json",
		"h1/state.vscdb",
		"h1/chatSessions/s1.jsonl",
		"h1/GitHub.copilot-chat/transcripts/s1.jsonl",
		"h1/GitHub.copilot-chat/debug-logs/s1/main.jsonl",
		"h1/chatEditingSessions/s1/state.json",
		"h1/chatEditingSessions/s1/contents/a1b2",
		"h1/chatEditingSessions/s1/contents/shot.png",
		"h1/ms-python.python/cache.json",
	} {
		write(t, filepath.Join(root, rel), `{"a":1}`+"\n")
	}
	for id, want := range map[string][]string{
		"copilot-vscode-transcripts":   {"h1/GitHub.copilot-chat/transcripts/s1.jsonl"},
		"copilot-vscode-chat-sessions": {"h1/chatSessions/s1.jsonl"},
		"copilot-vscode-editing":       {"h1/chatEditingSessions/s1/contents/a1b2", "h1/chatEditingSessions/s1/state.json"},
	} {
		if got := collected(t, id, root, nil); !slices.Equal(got, want) {
			t.Errorf("%s collected %v, want %v", id, got, want)
		}
	}
}

func TestCopilotVersionIsObserved(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "session-state", "s1", "events.jsonl"),
		`{"type":"session.start","data":{"sessionId":"s1","version":1,"producer":"copilot-agent","copilotVersion":"1.0.88"},"id":"e0","parentId":null}`+"\n")

	d := discover(t, source(root, []string{"session-state/*/events.jsonl"}), nil)
	if d.AgentVersion != "1.0.88" {
		t.Errorf("agent version %q, want 1.0.88", d.AgentVersion)
	}
}
