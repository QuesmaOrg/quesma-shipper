package e2e

import (
	"database/sql"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Fixtures are built, never copied from a machine: a captured transcript carries prose nobody has
// read line by line, and the Cursor pair (a transcript plus SQLite rows) can only be kept in
// agreement by generating both from one description. Each builder is named for the format
// generation it represents; a vendor changing shape gets a NEW builder, never an edit to this one.

// Deliberately planted in the fixtures: the engine falls back to os/user when the state directory
// is a temp path naming nobody, and this is what makes TestNoShippedByteCarriesTheOSUsername real.
func realUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil || len(u.Username) < 2 {
		t.Skip("no usable OS username; the placeholder cannot be exercised")
	}
	// Windows usernames come as HOST\name; we only care about the name.
	if _, name, ok := strings.Cut(u.Username, `\`); ok {
		return name
	}
	return u.Username
}

// Shapes the compiled rule packs recognise, lifted from the redaction conformance vectors, so a
// rule that stops matching fails unambiguously rather than on a fixture that never matched.
const (
	seededGitHubToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	seededAWSKey      = "AKIAIOSFODNN7EXAMPLE"

	// No recognisable shape: only the key name can catch this one.
	seededShapelessSecret = "zx81plainvaluenoshapeatall"
)

// Hex with letters, because the card-pan rule eats an all-digit id: see
// TestGolden/claude-2026-07-numeric-session, which keeps that case on the record.
const claudeSessionID = "9f8b7c6d-4e3a-4b2c-8d1e-0f9a8b7c6d5e"

// A session id with no hex letters at all: rare, and possible.
const numericSessionID = "11111111-1111-4111-8111-111111111111"

// One Claude Code transcript: a user turn, an assistant turn with usage, a tool call whose output
// leaks a key, and a summary. Small on purpose, so a reader can reason about a failure.
func claudeSession2026_07(username, sessionID string) string {
	cwd := "/Users/" + username + "/work/demo"
	lines := []string{
		fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":%q,"cwd":%q,"message":{"role":"user","content":[{"type":"text","text":"deploy the api"}]}}`, sessionID, cwd),
		fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":%q,"cwd":%q,"message":{"id":"m1","model":"claude-opus-5","content":[{"type":"text","text":"Using %s to authenticate."}],"usage":{"input_tokens":120,"output_tokens":340,"cache_read_input_tokens":9000}}}`, sessionID, cwd, seededGitHubToken),
		fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":%q,"cwd":%q,"message":{"role":"user","content":[{"type":"tool_result","content":"AWS_ACCESS_KEY_ID=%s"}]}}`, sessionID, cwd, seededAWSKey),
		// Shapeless credentials beside the token counts that must survive redaction.
		fmt.Sprintf(`{"type":"assistant","uuid":"a2","sessionId":%q,"cwd":%q,"message":{"id":"m2","model":"claude-opus-5","content":[{"type":"text","text":"retrying"}],"api_key":%q,"password":%q,"usage":{"input_tokens":7,"output_tokens":11,"cache_read_input_tokens":9000}}}`,
			sessionID, cwd, seededShapelessSecret, seededShapelessSecret),
	}
	return strings.Join(lines, "\n") + "\n"
}

// stageClaude puts that transcript where the catalog looks for it and returns its path.
func stageClaude(t *testing.T, w *world, username string) string {
	return stageClaudeSession(t, w, username, claudeSessionID)
}

func stageClaudeSession(t *testing.T, w *world, username, sessionID string) string {
	t.Helper()
	// Claude's encoded-cwd slug carries the username in the other shape the placeholder handles:
	// "-Users-jane-work-demo", not "/Users/jane/work/demo".
	slug := "-Users-" + username + "-work-demo"
	rel := filepath.ToSlash(filepath.Join(".claude", "projects", slug, sessionID+".jsonl"))
	return stageFile(t, w, rel, claudeSession2026_07(username, sessionID))
}

// --- cursor -------------------------------------------------------------------

// The conversation id appears in the file name and the store keys but nowhere inside the
// transcript, which is why the join exists and why the ETL recovers it from the path.
const cursorConv = "c8cbeb0b-7950-4b1d-a202-dc3351032bfa"

// One description compiled into both halves, the JSONL line and the SQLite bubble row, so the two
// cannot disagree.
type cursorTurn struct {
	BubbleID string
	Kind     int // 1 user, 2 assistant
	Text     string
	Model    string
	Tool     string // non-empty: this turn is a tool call
	Command  string
	Result   string // what only the store has

	// NoBubble compiles the transcript line without its store half: the store deduplicated a
	// command the agent ran twice.
	NoBubble bool
}

// A conversation the store deduplicated: the agent ran one command twice and only the first run
// has a bubble row.
func cursorConversationRepeat() []cursorTurn {
	return []cursorTurn{
		{BubbleID: "r1", Kind: 1, Text: "run the tests, then run them again"},
		{
			BubbleID: "r2", Kind: 2, Tool: "run_terminal_cmd",
			Command: "go test ./internal/...",
			Result:  "ok  \tdemo/internal/api\t0.24s",
		},
		// Byte-identical to the run above and recorded nowhere: the store kept one row for both.
		{
			BubbleID: "r3", Kind: 2, Tool: "run_terminal_cmd",
			Command:  "go test ./internal/...",
			NoBubble: true,
		},
		{BubbleID: "r4", Kind: 2, Text: "Both runs passed.", Model: "claude-opus-5"},
	}
}

func cursorConversation2026_07() []cursorTurn {
	return []cursorTurn{
		{BubbleID: "b1", Kind: 1, Text: "list the workspace"},
		{BubbleID: "b2", Kind: 2, Text: "Listing the workspace folder contents.", Model: "claude-opus-5"},
		{
			BubbleID: "b3", Kind: 2, Tool: "run_terminal_cmd",
			Command: "ls -la /work/api",
			// The store's exclusive contribution: the transcript never records what a command printed.
			Result: "total 24\n-rw-r--r--  1 dev staff  812 Jul  1 09:58 main.go",
		},
	}
}

// withStore=false is the drift case: a transcript whose bubbles the store does not have.
func stageCursor(t *testing.T, w *world, username string, turns []cursorTurn, withStore bool) string {
	t.Helper()

	var lines []string
	for _, turn := range turns {
		switch {
		case turn.Tool != "":
			lines = append(lines, fmt.Sprintf(
				`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":%q}}]}}`,
				turn.Command))
		case turn.Kind == 1:
			lines = append(lines, fmt.Sprintf(
				`{"role":"user","message":{"content":[{"type":"text","text":%q}]}}`, turn.Text))
		default:
			lines = append(lines, fmt.Sprintf(
				`{"role":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, turn.Text))
		}
	}
	lines = append(lines, `{"type":"turn_ended","status":"success"}`)

	rel := filepath.ToSlash(filepath.Join(".cursor", "projects", "Users-"+username+"-work-demo",
		"agent-transcripts", cursorConv, cursorConv+".jsonl"))
	path := stageFile(t, w, rel, strings.Join(lines, "\n")+"\n")

	if withStore {
		writeCursorStore(t, w, turns)
	}
	return path
}

// The fixture holds only what the enricher's declared read scope covers, so a widened scope shows
// up as a test that reads nothing new rather than one quietly collecting more.
func writeCursorStore(t *testing.T, w *world, turns []cursorTurn) {
	t.Helper()
	path := filepath.Join(w.Home, filepath.FromSlash(cursorStatePath()))
	if err := ensureDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`,
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	var headers []string
	for _, turn := range turns {
		if !turn.NoBubble {
			headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":%d}`, turn.BubbleID, turn.Kind))
		}
	}
	insert := func(key, value string) {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	insert("composerData:"+cursorConv, fmt.Sprintf(
		`{"composerId":%q,"createdAt":1751000000000,"fullConversationHeadersOnly":[%s]}`,
		cursorConv, strings.Join(headers, ",")))

	for i, turn := range turns {
		if turn.NoBubble {
			continue
		}
		created := fmt.Sprintf("2026-07-01T09:0%d:00.000Z", i)
		switch {
		case turn.Tool != "":
			insert("bubbleId:"+cursorConv+":"+turn.BubbleID, fmt.Sprintf(
				`{"bubbleId":%q,"type":2,"createdAt":%q,"toolFormerData":{"toolCallId":"call_%s","name":%q,"status":"completed","rawArgs":"{\"command\":\"%s\"}","result":%q}}`,
				turn.BubbleID, created, turn.BubbleID, turn.Tool, turn.Command, turn.Result))
		default:
			model := ""
			if turn.Model != "" {
				model = fmt.Sprintf(`,"modelName":%q`, turn.Model)
			}
			insert("bubbleId:"+cursorConv+":"+turn.BubbleID, fmt.Sprintf(
				`{"bubbleId":%q,"type":%d,"text":%q,"createdAt":%q,"requestId":"req-%s"%s}`,
				turn.BubbleID, turn.Kind, turn.Text, created, turn.BubbleID, model))
		}
	}
	// The enricher records DB provenance, so an unstable mtime would reach the manifest.
	touch(t, path)
}
