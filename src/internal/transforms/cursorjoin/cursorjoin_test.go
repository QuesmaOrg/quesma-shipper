package cursorjoin_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
)

// The fixtures are synthesised to the observed shapes, not captured: a real state.vscdb holds
// someone's conversations and their session tokens. Every assertion below is about a documented
// behaviour, so a Cursor change that breaks the join breaks these tests for the right reason.

const conv = "5d1f7b3e-9a2c-4e8f-b1d0-3c4a5b6c7d8e"

type storeRow struct {
	key   string
	value string
}

func newStore(t *testing.T, rows []storeRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, r.key, r.value); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
	}
	return path
}

// transcript is the raw side: a user turn, an assistant turn with a tool call, a terminator.
const transcript = `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Tuesday, Jul 28, 2026, 12:06 PM (UTC+2)</timestamp>\n<user_query>\nlist the workspace\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Listing the workspace folder contents."},{"type":"tool_use","name":"Shell","input":{"command":"ls -la /work/api","description":"List files in workspace root"}}]}}
{"type":"turn_ended","status":"success"}
`

// fullStore is the happy path: headers name every bubble, the tool bubble carries the result.
func fullStore(t *testing.T) string {
	t.Helper()
	return newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1753700000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"b2","type":2},
					{"bubbleId":"b3","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"list the workspace",
				"createdAt":"2026-07-28T10:06:00.000Z","requestId":"req-1"}`,
		},
		{
			key: "bubbleId:" + conv + ":b2",
			value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents.",
				"createdAt":"2026-07-28T10:06:01.000Z","requestId":"req-2",
				"modelName":"claude-4.5-sonnet","turnDurationMs":1420,"checkpointId":"ckpt-9"}`,
		},
		{
			// The point of the enricher: the store has the output, the call id, the status.
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"createdAt":"2026-07-28T10:06:02.000Z",
				"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
					"status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
					"result":"total 24\ndrwxr-xr-x  5 jane staff  160 Jul 28 10:05 .\n-rw-r--r--  1 jane staff  812 Jul 28 09:58 main.go"}}`,
		},
	})
}

func unit(t *testing.T, body string) transforms.RawUnit {
	t.Helper()
	return transforms.RawUnit{
		NativePath: "/Users/jane/.cursor/projects/api/agent-transcripts/" + conv + ".jsonl",
		Content:    []byte(body),
		SourceHash: transforms.Hash([]byte(body)),
	}
}

func run(t *testing.T, dbPath string, units ...transforms.RawUnit) transforms.EnrichResult {
	t.Helper()
	return cursorjoin.New().Enrich(transforms.Input{
		SourceID:   "cursor-transcripts",
		Units:      units,
		DBPath:     dbPath,
		ScratchDir: filepath.Join(t.TempDir(), "scratch"),
	})
}

// decode reads the derived JSONL back.
func decode(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("derived line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// THE JOIN. What the transcript cannot say, the derived object does.
func TestTheJoinCarriesTheFieldsTheTranscriptLacks(t *testing.T) {
	res := run(t, fullStore(t), unit(t, transcript))

	if res.Mismatched != 0 {
		t.Fatalf("the happy path mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("expected 1 derived object, got %d: %v", len(res.Objects), res.Notes)
	}
	d := res.Objects[0]
	if d.Status != transforms.StatusOK {
		t.Fatalf("status = %s", d.Status)
	}
	if !strings.HasSuffix(d.NativePath, conv+".jsonl.enriched.jsonl") {
		t.Errorf("derived path = %s", d.NativePath)
	}

	lines := decode(t, d.Payload)
	if len(lines) != 3 {
		t.Fatalf("derived %d lines, want 3 (one per transcript line)", len(lines))
	}

	// The assistant turn: block 0 is prose, block 1 is the tool call.
	blocks, ok := lines[1]["_enrich"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("assistant line has no per-block enrichment: %v", lines[1]["_enrich"])
	}
	tool, ok := blocks[1].(map[string]any)
	if !ok {
		t.Fatalf("the tool_use block was not enriched: %v", blocks[1])
	}

	// The four fields the raw transcript structurally cannot carry.
	if tool["tool_call_id"] != "call_abc123" {
		t.Errorf("tool_call_id = %v", tool["tool_call_id"])
	}
	if tool["status"] != "completed" {
		t.Errorf("status = %v", tool["status"])
	}
	if tool["tool_name"] != "run_terminal_cmd" {
		t.Errorf("tool_name = %v (the INTERNAL name, not the transcript's display name)", tool["tool_name"])
	}
	if s, _ := tool["result"].(string); !strings.Contains(s, "main.go") {
		t.Errorf("the tool result did not make it into the derived object: %v", tool["result"])
	}
	// A real timestamp, where the transcript's only time signal is prose in a user message.
	if tool["createdAt"] != "2026-07-28T10:06:02.000Z" {
		t.Errorf("createdAt = %v", tool["createdAt"])
	}
}

// The native lines survive byte-for-byte.
func TestTheNativeTranscriptIsPreservedVerbatim(t *testing.T) {
	res := run(t, fullStore(t), unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}

	want := strings.Split(strings.TrimRight(transcript, "\n"), "\n")
	for i, line := range strings.Split(strings.TrimRight(string(res.Objects[0].Payload), "\n"), "\n") {
		var m struct {
			Native json.RawMessage `json:"native"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		// Byte-identical, not merely equivalent: downstream verifies the derived object
		// against the raw file.
		if string(m.Native) != want[i] {
			t.Errorf("line %d was re-encoded:\n got %s\nwant %s", i, m.Native, want[i])
		}
	}
}

func TestDerivedFromNamesTheRawInput(t *testing.T) {
	u := unit(t, transcript)
	res := run(t, fullStore(t), u)
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	d := res.Objects[0]
	// Without the pairing, a derived object is an assertion nobody can check.
	if len(d.DerivedFrom) != 1 || d.DerivedFrom[0] != u.SourceHash {
		t.Errorf("derived_from = %v, want [%s]", d.DerivedFrom, u.SourceHash)
	}
	if d.DBReadMethod == "" || len(d.DBKeyspaces) == 0 || d.DBRowsRead == 0 {
		t.Errorf("DB provenance is incomplete: method=%q keyspaces=%v rows=%d",
			d.DBReadMethod, d.DBKeyspaces, d.DBRowsRead)
	}
	// Scope is declared, and the declaration is what the provenance records.
	for _, ks := range d.DBKeyspaces {
		if ks != "composerData:" && ks != "bubbleId:" {
			t.Errorf("undeclared keyspace in provenance: %s", ks)
		}
	}
}

// THE DETERMINISM GATE. Same input values, same output bytes.
func TestTheOutputIsByteIdenticalAcrossRuns(t *testing.T) {
	// Two databases with the same rows inserted in a different order. If the output differed,
	// the hash would stop being a change signal and every tick would re-ship everything.
	first := run(t, fullStore(t), unit(t, transcript))
	second := run(t, fullStore(t), unit(t, transcript))

	if len(first.Objects) != 1 || len(second.Objects) != 1 {
		t.Fatalf("expected one object each: %d, %d", len(first.Objects), len(second.Objects))
	}
	if string(first.Objects[0].Payload) != string(second.Objects[0].Payload) {
		t.Error("two runs over identical inputs produced different bytes")
	}
	if first.Objects[0].OutputHash != second.Objects[0].OutputHash {
		t.Errorf("output hashes differ: %s vs %s",
			first.Objects[0].OutputHash, second.Objects[0].OutputHash)
	}
}

func TestAChangedStoreChangesTheOutputHash(t *testing.T) {
	before := run(t, fullStore(t), unit(t, transcript))

	// A DB-side-only update: the transcript is byte-identical, so only the output hash can
	// signal that there is something new to ship.
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
				"result":"total 24\nA LATE RESULT ARRIVED"}}`,
		},
	}
	after := run(t, newStore(t, rows), unit(t, transcript))

	if len(before.Objects) != 1 || len(after.Objects) != 1 {
		t.Fatalf("expected one object each: %v / %v", before.Notes, after.Notes)
	}
	if before.Objects[0].OutputHash == after.Objects[0].OutputHash {
		t.Error("a DB-side-only change did not change the output hash, so it would never ship")
	}
	if !strings.Contains(string(after.Objects[0].Payload), "A LATE RESULT ARRIVED") {
		t.Error("the late result is not in the derived object")
	}
}

// THE MISMATCH GATE: no derived entry, loud alarm, raw ships regardless.
func TestAnUnalignedTranscriptProducesNoDerivedObjectAndAnAlarm(t *testing.T) {
	// A store describing a different conversation: what a drifted join looks like.
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"b1","type":1},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"something else entirely"}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_zzz",
				"name":"read_file","rawArgs":"{\"path\":\"/etc/hosts\"}","result":"unrelated"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))

	// No derived entry: a partial one is indistinguishable downstream from a complete one.
	if len(res.Objects) != 0 {
		t.Fatalf("a mismatched join still produced %d derived objects", len(res.Objects))
	}
	if res.Mismatched != 1 {
		t.Errorf("mismatch count = %d, want 1", res.Mismatched)
	}
	if len(res.Notes) == 0 {
		t.Error("a mismatch produced no note: it has to be loud, not silent")
	}
	// The note must say what was lost, since the operator's next question is what to do.
	joined := strings.Join(res.Notes, " ")
	if !strings.Contains(joined, "raw transcript ships") {
		t.Errorf("the note does not say the raw file is unaffected: %q", joined)
	}
}

// THE COMPACTION FIXTURE. summarizedComposers is the scenario most likely to break the join.
func TestACompactedConversationStillEnriches(t *testing.T) {
	// After compaction the header list still names bubbles whose rows are gone. Counting a
	// missing row as a mismatch would stop DB-side collection for the longest conversations.
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,
				"summarizedComposers":[{"summary":"earlier turns were compacted away"}],
				"fullConversationHeadersOnly":[
					{"bubbleId":"gone1","type":1},
					{"bubbleId":"gone2","type":2},
					{"bubbleId":"b1","type":1},
					{"bubbleId":"b2","type":2},
					{"bubbleId":"b3","type":2}]}`, conv),
		},
		// gone1 and gone2 have no rows: compaction removed them.
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
				"result":"total 24"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))

	if res.Mismatched != 0 {
		t.Fatalf("a compacted conversation mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("a compacted conversation produced no derived object: %v", res.Notes)
	}
	if !strings.Contains(string(res.Objects[0].Payload), "call_abc123") {
		t.Error("the surviving turns were not enriched")
	}
}

// The draft skip rule: most composerData rows on a real machine are drafts.
func TestADraftConversationIsSkippedNotMismatched(t *testing.T) {
	rows := []storeRow{{
		key:   "composerData:" + conv,
		value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[],"conversation":[]}`, conv),
	}}
	res := run(t, newStore(t, rows), unit(t, transcript))

	// Skipped, not mismatched: only one of the two is an alarm, and drafts would bury it.
	if res.Mismatched != 0 {
		t.Errorf("a draft was counted as a mismatch: %v", res.Notes)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
	if len(res.Objects) != 0 {
		t.Errorf("a draft produced %d derived objects", len(res.Objects))
	}
}

func TestScaffoldingAndReasoningBubblesAreNotEvents(t *testing.T) {
	// These have no transcript counterpart; left in the event list they shift every alignment.
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"cap","type":2},
				{"bubbleId":"b1","type":1},
				{"bubbleId":"think","type":2},
				{"bubbleId":"b2","type":2},
				{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":cap", value: `{"bubbleId":"cap","type":2,"isCapabilityIteration":true,"capabilityType":"tool-negotiation"}`},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{key: "bubbleId:" + conv + ":think", value: `{"bubbleId":"think","type":2,"isThought":true,"text":"internal reasoning"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("scaffolding bubbles broke the alignment: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	// And the scaffolding must not appear as enrichment on a real block.
	if strings.Contains(string(res.Objects[0].Payload), "tool-negotiation") {
		t.Error("a scaffolding bubble leaked into the derived object")
	}
	if strings.Contains(string(res.Objects[0].Payload), "internal reasoning") {
		t.Error("a thinking-only bubble leaked into the derived object")
	}
}

// The current store generation, four drifts at once: numeric tool and capabilityType enums,
// capabilityType 15 on every tool bubble, a thinking object rather than isThought, and modelName
// under modelInfo. A struct that rejects any of them drops the row and reports it as a mismatch.
func TestTheCurrentStoreGenerationEnriches(t *testing.T) {
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1753700000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"u1","type":1},
					{"bubbleId":"think1","type":2},
					{"bubbleId":"a1","type":2},
					{"bubbleId":"tool1","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":u1",
			value: `{"bubbleId":"u1","type":1,"text":"list the workspace",
				"createdAt":"2026-07-28T10:06:09.499Z","requestId":"req-1"}`,
		},
		{
			// The reasoning bubble: capabilityType 30, a thinking object, no isThought.
			key: "bubbleId:" + conv + ":think1",
			value: `{"bubbleId":"think1","type":2,"text":"","capabilityType":30,
				"thinking":{"text":"the user wants a listing","signature":"sig"},
				"createdAt":"2026-07-28T10:06:11.463Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":a1",
			value: `{"bubbleId":"a1","type":2,"text":"Listing the workspace folder contents.\n\n\n",
				"createdAt":"2026-07-28T10:06:11.477Z","modelInfo":{"modelName":"composer-2.5"}}`,
		},
		{
			// capabilityType 15 and toolFormerData, numeric tool, arguments in params.
			key: "bubbleId:" + conv + ":tool1",
			value: `{"bubbleId":"tool1","type":2,"text":"","capabilityType":15,
				"createdAt":"2026-07-28T10:06:11.516Z",
				"toolFormerData":{"toolCallId":"tool_33e5b6ee","name":"run_terminal_command_v2",
					"tool":15,"status":"completed","rawArgs":"",
					"params":"{\"command\":\"ls -la /work/api\",\"cwd\":\"\"}",
					"result":"{\"output\":\"total 24\\nmain.go\"}"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))

	if res.Mismatched != 0 {
		t.Fatalf("the current store generation mismatched: %v", res.Notes)
	}
	// No decode-failure note either: every fixture row must decode.
	for _, n := range res.Notes {
		if strings.Contains(n, "did not decode") {
			t.Fatalf("current-generation rows failed to decode: %v", res.Notes)
		}
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)

	blocks, ok := lines[1]["_enrich"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("assistant line has no per-block enrichment: %v", lines[1]["_enrich"])
	}
	prose, _ := blocks[0].(map[string]any)
	if prose == nil || prose["modelName"] != "composer-2.5" {
		t.Errorf("modelInfo.modelName did not reach the derived object: %v", blocks[0])
	}
	tool, _ := blocks[1].(map[string]any)
	if tool == nil {
		t.Fatalf("the tool_use block was not enriched: %v", blocks[1])
	}
	if tool["tool_call_id"] != "tool_33e5b6ee" {
		t.Errorf("tool_call_id = %v", tool["tool_call_id"])
	}
	if tool["tool_name"] != "run_terminal_command_v2" {
		t.Errorf("tool_name = %v", tool["tool_name"])
	}
	if s, _ := tool["result"].(string); !strings.Contains(s, "main.go") {
		t.Errorf("the tool result did not make it into the derived object: %v", tool["result"])
	}
	// The thinking bubble must not leak: [REDACTED] leaves nothing to attach it to.
	if strings.Contains(string(res.Objects[0].Payload), "the user wants a listing") {
		t.Error("a thinking bubble leaked into the derived object")
	}
}

// The current transcript generation: reasoning as a plain text block, terminal bubbles with no
// recorded arguments, and injected follow-up turns that never get bubble rows.
func TestTheCurrentTranscriptGenerationEnriches(t *testing.T) {
	// Line 2's first block is the thinking text verbatim; lines 5+ are the injected turns.
	current := `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:00 AM (UTC+2)</timestamp>\n<user_query>\nstart the dev server\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"I need to check if a dev server is already running, then start it."},{"type":"tool_use","name":"Shell","input":{"command":"pnpm dev","description":"Start the dev server"}}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"The server is up on port 5173."}]}}
{"type":"turn_ended","status":"success"}
{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:05 AM (UTC+2)</timestamp>\n\n<user_query>Briefly inform the user about the task result.</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"The dev server is running."}]}}
{"type":"turn_ended","status":"success"}
`
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"u1","type":1},
				{"bubbleId":"think1","type":2},
				{"bubbleId":"tool1","type":2},
				{"bubbleId":"a1","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":u1", value: `{"bubbleId":"u1","type":1,"text":"start the dev server"}`},
		{
			key: "bubbleId:" + conv + ":think1",
			value: `{"bubbleId":"think1","type":2,"text":"","capabilityType":30,
				"thinking":{"text":"I need to check if a dev server is already running, then start it."},
				"createdAt":"2026-08-03T07:00:01.000Z"}`,
		},
		{
			// No recorded arguments: position and a compatible name are the evidence.
			key: "bubbleId:" + conv + ":tool1",
			value: `{"bubbleId":"tool1","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"tool_dev123","name":"run_terminal_command_v2",
					"tool":15,"status":"completed","rawArgs":"{}","params":"",
					"result":"{\"output\":\"VITE ready on :5173\"}"}}`,
		},
		{key: "bubbleId:" + conv + ":a1", value: `{"bubbleId":"a1","type":2,"text":"The server is up on port 5173."}`},
	}
	res := run(t, newStore(t, rows), transforms.RawUnit{
		NativePath: "/Users/jane/.cursor/projects/api/agent-transcripts/" + conv + ".jsonl",
		Content:    []byte(current),
		SourceHash: transforms.Hash([]byte(current)),
	})

	if res.Mismatched != 0 {
		t.Fatalf("the current transcript generation mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	payload := string(res.Objects[0].Payload)

	// The thinking text block matched the thinking bubble rather than mismatching.
	lines := decode(t, res.Objects[0].Payload)
	blocks, ok := lines[1]["_enrich"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("assistant line has no per-block enrichment: %v", lines[1]["_enrich"])
	}
	think, _ := blocks[0].(map[string]any)
	if think == nil || think["bubbleId"] != "think1" {
		t.Errorf("the thinking text block did not align to the thinking bubble: %v", blocks[0])
	}
	// The no-arguments tool bubble matched by position and name, carrying the result.
	tool, _ := blocks[1].(map[string]any)
	if tool == nil || tool["tool_call_id"] != "tool_dev123" {
		t.Errorf("the no-arguments tool bubble did not align: %v", blocks[1])
	}
	if !strings.Contains(payload, "VITE ready") {
		t.Error("the tool result did not reach the derived object")
	}
	// The injected trailing turns are tail, not mismatch: carried native-only, said out
	// loud as an info — the object shipped, so it must not read as loss in Notes.
	joined := strings.Join(res.Infos, " ")
	if !strings.Contains(joined, "extend past the store") {
		t.Errorf("the injected trailing turns produced no tail info: %v", res.Infos)
	}
	if got := len(lines); got != 7 {
		t.Errorf("derived %d lines, want 7 (every native line carried)", got)
	}
}

// A transcript that matched NOTHING must never ride out on the tail rule.
func TestAWhollyUnmatchedTranscriptIsAMismatchNotATail(t *testing.T) {
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"b1","type":1},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"something else entirely"}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_zzz",
				"name":"read_file","rawArgs":"{\"path\":\"/etc/hosts\"}","result":"unrelated"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))
	if res.Mismatched != 1 {
		t.Errorf("mismatch count = %d, want 1: %v", res.Mismatched, res.Notes)
	}
	if len(res.Objects) != 0 {
		t.Errorf("a wholly unmatched transcript still derived %d objects", len(res.Objects))
	}
}

// Within one turn the store's header order and the transcript's block order can disagree, so
// the match must land behind the cursor, on positive argument evidence only.
func TestAnOutOfOrderToolBubbleIsFoundBehindTheCursor(t *testing.T) {
	body := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>check types</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Spawning a typecheck subagent."},{"type":"tool_use","name":"Task","input":{"description":"Typecheck the repo","prompt":"Run npx tsc --noEmit in the repo and report errors."}}]}}
{"type":"turn_ended","status":"success"}
`
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"u1","type":1},
				{"bubbleId":"task1","type":2},
				{"bubbleId":"a1","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":u1", value: `{"bubbleId":"u1","type":1,"text":"check types"}`},
		{
			// The store puts the task bubble before the prose, so the prose match
			// advances the cursor past it.
			key: "bubbleId:" + conv + ":task1",
			value: `{"bubbleId":"task1","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"task_777","name":"task_v2","tool":38,
					"status":"completed",
					"params":"{\"description\":\"Typecheck the repo\",\"prompt\":\"Run npx tsc --noEmit in the repo and report errors.\"}",
					"result":"no type errors"}}`,
		},
		{key: "bubbleId:" + conv + ":a1", value: `{"bubbleId":"a1","type":2,"text":"Spawning a typecheck subagent."}`},
	}
	res := run(t, newStore(t, rows), transforms.RawUnit{
		NativePath: "/Users/jane/.cursor/projects/api/agent-transcripts/" + conv + ".jsonl",
		Content:    []byte(body),
		SourceHash: transforms.Hash([]byte(body)),
	})
	if res.Mismatched != 0 {
		t.Fatalf("the out-of-order tool bubble mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	if !strings.Contains(string(res.Objects[0].Payload), "task_777") {
		t.Error("the tool block did not align to the bubble behind the cursor")
	}
}

// A row that still fails to decode is counted out loud: silence surfaces only as a mismatch
// alarm pointing at alignment instead.
func TestUndecodableStoreRowsProduceANote(t *testing.T) {
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}}`,
		},
		// A bubble row whose shape drifted beyond what the struct tolerates.
		{key: "bubbleId:" + conv + ":bad", value: `{"bubbleId":{"not":"a string"},"type":2}`},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))

	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "did not decode") {
			found = true
		}
	}
	if !found {
		t.Errorf("an undecodable row produced no note: %v", res.Notes)
	}
	// And the rest of the conversation still enriches: fail open applies row by row.
	if len(res.Objects) != 1 {
		t.Fatalf("an undecodable row cost the whole conversation: %v", res.Notes)
	}
}

func TestTheBubbleScanFallbackOrdersByCreatedAt(t *testing.T) {
	// The header list is empty, as observed on errored turns, so the fallback orders by
	// createdAt, the only real timestamp available.
	rows := []storeRow{
		{key: "composerData:" + conv, value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[]}`, conv)},
		{key: "bubbleId:" + conv + ":zzz", value: `{"bubbleId":"zzz","type":2,"text":"Listing the workspace folder contents.","createdAt":"2026-07-28T10:06:01Z"}`},
		{key: "bubbleId:" + conv + ":aaa", value: `{"bubbleId":"aaa","type":1,"text":"list the workspace","createdAt":"2026-07-28T10:06:00Z"}`},
		{
			key: "bubbleId:" + conv + ":mmm",
			value: `{"bubbleId":"mmm","type":2,"createdAt":"2026-07-28T10:06:02Z",
				"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
					"rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the fallback ordering mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	// Key order alone would have put aaa, mmm, zzz: createdAt is what makes this work.
	if !strings.Contains(string(res.Objects[0].Payload), "call_abc123") {
		t.Error("the tool bubble did not align under the fallback ordering")
	}
}

// A truncated tail is expected, not a failure.
func TestATruncatedTranscriptTailStillEnrichesWhatCameBefore(t *testing.T) {
	// Writes are not atomic: complete lines enrich, the fragment passes through unenriched.
	torn := transcript + `{"role":"assistant","message":{"content":[{"type":"te`
	res := run(t, fullStore(t), unit(t, torn))

	if len(res.Objects) != 1 {
		t.Fatalf("a torn tail lost the whole conversation: %v", res.Notes)
	}
	payload := string(res.Objects[0].Payload)
	if !strings.Contains(payload, "call_abc123") {
		t.Error("the complete lines were not enriched")
	}
	// Preserved as a string, not dropped: an invalid raw value would cost the conversation,
	// and shortening the view would disagree with the transcript about where the file ended.
	if !strings.Contains(payload, "native_invalid") {
		t.Error("the torn fragment was dropped from the derived object")
	}
	for _, l := range decode(t, res.Objects[0].Payload) {
		_ = l // decode fails the test if any derived line is not valid JSON
	}
}

// The transcript side of the store's enum drift: a strict string field would reject the whole
// line, which then ships as native_invalid with its enrichment gone and mismatches at zero.
func TestATypeDriftedTranscriptLineStillDecodesAndEnriches(t *testing.T) {
	// The assistant line's role and the terminator's status arrive as numbers.
	drifted := `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Tuesday, Jul 28, 2026, 12:06 PM (UTC+2)</timestamp>\n<user_query>\nlist the workspace\n</user_query>"}]}}
{"role":2,"message":{"content":[{"type":"text","text":"Listing the workspace folder contents."},{"type":"tool_use","name":"Shell","input":{"command":"ls -la /work/api","description":"List files in workspace root"}}]}}
{"type":"turn_ended","status":3}
`
	res := run(t, fullStore(t), unit(t, drifted))

	if res.Mismatched != 0 {
		t.Fatalf("the drifted lines mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	payload := string(res.Objects[0].Payload)
	if strings.Contains(payload, "native_invalid") {
		t.Error("a type-drifted line was filed as invalid JSON, which it is not")
	}
	if !strings.Contains(payload, "call_abc123") {
		t.Error("the drifted assistant line lost its enrichment")
	}
}

// The observed migration was string to number, but no other shape may cost the bubble either:
// a decode error in one field discards the whole row.
func TestAFlexFieldWithAnUnexpectedShapeDoesNotCostTheBubble(t *testing.T) {
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"fullConversationHeadersOnly":[
				{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			// capabilityType a bool, tool an object: shapes no store has written yet.
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"capabilityType":true,
				"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
					"tool":{"kind":15},"status":"completed",
					"rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))

	for _, n := range res.Notes {
		if strings.Contains(n, "did not decode") {
			t.Errorf("an unexpected scalar shape cost a whole bubble row: %s", n)
		}
	}
	if res.Mismatched != 0 {
		t.Fatalf("the bubble was lost and the loss surfaced as a mismatch: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	if !strings.Contains(string(res.Objects[0].Payload), "call_abc123") {
		t.Error("the tool bubble did not survive its drifted fields")
	}
}

// No database is not an error.
func TestNoDatabaseMeansSkippedNotFailed(t *testing.T) {
	res := run(t, "", unit(t, transcript))
	if res.Errors != 0 {
		t.Errorf("a missing state.vscdb was counted as an error")
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
	if len(res.Objects) != 0 {
		t.Errorf("derived %d objects with no database", len(res.Objects))
	}
}

// An unreadable database fails open.
func TestAnUnreadableDatabaseFailsOpenWithACount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := run(t, path, unit(t, transcript))

	// Fail open and counted: an unreadable database costs this flush's DB-side fields only.
	if len(res.Objects) != 0 {
		t.Errorf("derived %d objects from an unreadable database", len(res.Objects))
	}
	if res.Errors != 1 {
		t.Errorf("errors = %d, want 1", res.Errors)
	}
	if len(res.Notes) == 0 {
		t.Error("an unreadable database produced no note")
	}
}

// THE NO-ROWS GATE, at this level: nothing the enricher emits may contain a raw store row.
func TestNoStoreRowReachesTheDerivedObjectWholesale(t *testing.T) {
	// A row field the join does not use must not appear: the derived object is a join of
	// named fields, not a dump, which is where "no rows ship" could be violated.
	rows := []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"UNUSED_MARKER":"must-not-ship",
				"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}`, conv),
		},
		{key: "bubbleId:" + conv + ":b1", value: `{"bubbleId":"b1","type":1,"text":"list the workspace","ALSO_UNUSED":"must-not-ship"}`},
		{key: "bubbleId:" + conv + ":b2", value: `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}}`,
		},
	}
	res := run(t, newStore(t, rows), unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	payload := string(res.Objects[0].Payload)
	for _, marker := range []string{"UNUSED_MARKER", "ALSO_UNUSED", "must-not-ship"} {
		if strings.Contains(payload, marker) {
			t.Errorf("a store field the join does not use reached the derived object: %s", marker)
		}
	}
}

func TestTheEnricherIsIdentifiedByIDAndVersion(t *testing.T) {
	e := cursorjoin.New()
	// Both travel in every manifest, so a fixed join's output supersedes a broken one's.
	if e.ID() != "cursor-transcript-join" {
		t.Errorf("id = %q", e.ID())
	}
	if e.Version() != 4 {
		t.Errorf("version = %d", e.Version())
	}
	if e.Table() != "cursorDiskKV" {
		t.Errorf("table = %q", e.Table())
	}
	// The workspace state.vscdb is v1.1, deferred. Only the global store is declared.
	for _, c := range e.DBCandidates() {
		if strings.Contains(c, "workspaceStorage") {
			t.Errorf("the workspace store is declared but deferred to v1.1: %s", c)
		}
		if !strings.Contains(c, "globalStorage") {
			t.Errorf("candidate is not the global store: %s", c)
		}
	}
}

// One store holds reasoning in two encodings at once. Declaring the field an object makes
// json.Unmarshal reject the whole row of every server-hydrated thought.
func TestServerHydratedReasoningIsDecodedNotDropped(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nwhy is the loader bounded\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Checking where the loader's bounds are set."},{"type":"text","text":"The bound is the row cap."}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"b2","type":2},
					{"bubbleId":"b3","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"why is the loader bounded",
				"createdAt":"2026-08-18T12:41:29.000Z"}`,
		},
		{
			// Server-hydrated: thinking is a string holding the object.
			key: "bubbleId:" + conv + ":b2",
			value: `{"bubbleId":"b2","type":2,"capabilityType":30,
				"serverBubbleId":"srv-77","requestId":"req-2",
				"createdAt":"2026-08-18T12:41:29.766Z",
				"thinking":"{\"text\":\"Checking where the loader's bounds are set.\",\"isLastThinkingChunk\":true}"}`,
		},
		{
			// Streamed locally, in the same conversation: thinking is an object.
			key: "bubbleId:" + conv + ":b3",
			value: `{"bubbleId":"b3","type":2,"capabilityType":30,
				"thinkingStyle":1,"requestId":"req-3",
				"createdAt":"2026-08-18T12:41:49.000Z",
				"thinking":{"text":"The bound is the row cap.","signature":"sig-abc"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	for _, n := range res.Notes {
		if strings.Contains(n, "did not decode") {
			t.Errorf("a server-hydrated thought was rejected as undecodable: %s", n)
		}
	}
	if res.Mismatched > 0 {
		t.Fatalf("mismatched %d: %v", res.Mismatched, res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, notes %v", len(res.Objects), res.Notes)
	}

	// Both reasoning bubbles carry their provenance, whichever writer produced them.
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"b2", "b3"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
}

// A short argument value is a substring of half the store, so comparing whole values is what
// refuses the bubble that merely starts with the same characters.
func TestACoincidentalArgumentMatchDoesNotStealADistantBubble(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfind the deny rules\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"deny","glob":"**/*","output_mode":"files_with_matches"}}]}}
`
	headers := []string{`{"bubbleId":"b1","type":1}`, `{"bubbleId":"near","type":2}`}
	rows := []storeRow{
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"find the deny rules",
				"createdAt":"2026-08-18T13:59:00.000Z"}`,
		},
		{
			// The real bubble, recorded without arguments: positional evidence only.
			key: "bubbleId:" + conv + ":near",
			value: `{"bubbleId":"near","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:59:01.000Z",
				"toolFormerData":{"toolCallId":"call_near","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{}","result":"the right result"}}`,
		},
	}
	// Filler: enough real bubbles to put the coincidence beyond the forward window.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("fill%02d", i)
		headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":2}`, id))
		rows = append(rows, storeRow{
			key: "bubbleId:" + conv + ":" + id,
			value: fmt.Sprintf(`{"bubbleId":%q,"type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:59:%02d.000Z",
				"toolFormerData":{"toolCallId":"call_%s","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/f%02d.go\"}",
					"result":"unrelated"}}`, id, i+2, id, i),
		})
	}
	// The coincidence: a DIFFERENT search, whose glob merely starts with the block's.
	headers = append(headers, `{"bubbleId":"far","type":2}`)
	rows = append(rows, storeRow{
		key: "bubbleId:" + conv + ":far",
		value: `{"bubbleId":"far","type":2,"capabilityType":15,
			"createdAt":"2026-08-18T13:59:40.000Z",
			"toolFormerData":{"toolCallId":"call_far","name":"ripgrep_raw_search",
				"status":"completed","rawArgs":"{\"pattern\":\"signed\",\"glob\":\"**/*.{md,json,yaml,go}\"}",
				"result":"the WRONG result"}}`,
	})
	rows = append(rows, storeRow{
		key: "composerData:" + conv,
		value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
			"fullConversationHeadersOnly":[%s]}`, conv, strings.Join(headers, ",")),
	})

	res := run(t, newStore(t, rows), unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "near" {
		t.Fatalf("the call joined to %v, want the bubble at its own position", e)
	}
	if e["result"] != "the right result" {
		t.Errorf("result = %v", e["result"])
	}
}

// A directory path is a prefix of every file path under it, so substring evidence made a search
// of a directory match a read of a file inside it. Whole-value comparison separates them.
func TestADirectoryArgumentDoesNotMatchAFileBeneathIt(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\naudit the config package\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"deny_additions","path":"/work/api/internal/config"}},{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"grep","type":2},
					{"bubbleId":"read","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"audit the config package",
				"createdAt":"2026-08-18T13:53:00.000Z"}`,
		},
		{
			// Recorded without arguments, so only its position speaks for it.
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:01.000Z",
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{}","result":"resolve.go:41"}}`,
		},
		{
			key: "bubbleId:" + conv + ":read",
			value: `{"bubbleId":"read","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:02.000Z",
				"toolFormerData":{"toolCallId":"call_read","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"grep", "read"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
}

// A call recorded without arguments can never produce positive evidence, so when the header order
// leaves its bubble behind the cursor, the nearest name-compatible one behind is the last resort.
func TestATerminalBubbleLeftBehindTheCursorIsStillFound(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nwhat changed\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}},{"type":"tool_use","name":"Shell","input":{"command":"git status --short"}}]}}
`
	db := newStore(t, []storeRow{
		{
			// The store's order is reversed here: the read match steps over the shell.
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"shell","type":2},
					{"bubbleId":"read","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"what changed",
				"createdAt":"2026-08-18T13:53:00.000Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":shell",
			value: `{"bubbleId":"shell","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:01.000Z",
				"toolFormerData":{"toolCallId":"call_shell","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{}","result":" M internal/config/resolve.go"}}`,
		},
		{
			key: "bubbleId:" + conv + ":read",
			value: `{"bubbleId":"read","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:02.000Z",
				"toolFormerData":{"toolCallId":"call_read","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"read", "shell"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
	// The result is what this exists for: the terminal output the transcript has none of.
	e, _ := enrich[1].(map[string]any)
	if e["result"] != " M internal/config/resolve.go" {
		t.Errorf("terminal result = %v", e["result"])
	}
}

// Distance, not discrimination: the distant bubble has the same arguments as the block, so only
// the window separates them. A conversation is not re-ordered by evidence.
func TestArgumentEvidenceDoesNotReachPastTheForwardWindow(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfind the deny rules\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"deny_additions","path":"/work/api/internal/config"}}]}}
`
	headers := []string{`{"bubbleId":"b1","type":1}`, `{"bubbleId":"near","type":2}`}
	rows := []storeRow{
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"find the deny rules",
				"createdAt":"2026-08-18T13:59:00.000Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":near",
			value: `{"bubbleId":"near","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:59:01.000Z",
				"toolFormerData":{"toolCallId":"call_near","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{}","result":"the right result"}}`,
		},
	}
	// Calls the transcript does not mention, putting the identical one out of the window.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("fill%02d", i)
		headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":2}`, id))
		rows = append(rows, storeRow{
			key: "bubbleId:" + conv + ":" + id,
			value: fmt.Sprintf(`{"bubbleId":%q,"type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:59:%02d.000Z",
				"toolFormerData":{"toolCallId":"call_%s","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/f%02d.go\"}",
					"result":"unrelated"}}`, id, i+2, id, i),
		})
	}
	// The same search much later: identical arguments, so only the window keeps the join off it.
	headers = append(headers, `{"bubbleId":"far","type":2}`)
	rows = append(rows,
		storeRow{
			key: "bubbleId:" + conv + ":far",
			value: `{"bubbleId":"far","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:59:40.000Z",
				"toolFormerData":{"toolCallId":"call_far","name":"ripgrep_raw_search",
					"status":"completed",
					"rawArgs":"{\"pattern\":\"deny_additions\",\"path\":\"/work/api/internal/config\"}",
					"result":"the WRONG result"}}`,
		},
		storeRow{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[%s]}`, conv, strings.Join(headers, ",")),
		})

	res := run(t, newStore(t, rows), unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "near" {
		t.Fatalf("the call joined to %v, want the bubble at its own position", e)
	}
	if e["result"] != "the right result" {
		t.Errorf("result = %v", e["result"])
	}
}

// Two argument-less shell bubbles behind the cursor, both name-compatible: nothing but position
// separates them. The nearest wins, and a change of tie-break shows up here.
func TestThePositionalLookBehindTakesTheNearestSiblingNotAnyOfThem(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nwhat changed\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}},{"type":"tool_use","name":"Shell","input":{"command":"git status --short"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"shellFar","type":2},
					{"bubbleId":"shellNear","type":2},
					{"bubbleId":"read","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"what changed",
				"createdAt":"2026-08-18T13:53:00.000Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":shellFar",
			value: `{"bubbleId":"shellFar","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:01.000Z",
				"toolFormerData":{"toolCallId":"call_far","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{}","result":"the FAR sibling"}}`,
		},
		{
			key: "bubbleId:" + conv + ":shellNear",
			value: `{"bubbleId":"shellNear","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:02.000Z",
				"toolFormerData":{"toolCallId":"call_near","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{}","result":" M internal/config/resolve.go"}}`,
		},
		{
			// Matching this one on its path carries the cursor past both shells.
			key: "bubbleId:" + conv + ":read",
			value: `{"bubbleId":"read","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:03.000Z",
				"toolFormerData":{"toolCallId":"call_read","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[1].(map[string]any)
	if e == nil || e["bubbleId"] != "shellNear" {
		t.Fatalf("the shell call joined to %v, want the nearest sibling behind the cursor", e)
	}
	if e["result"] == "the FAR sibling" {
		t.Error("the look-behind reached past a nearer sibling")
	}
}

// A thinking string that parses but not into a known field still carries prose: taking the text
// field alone would drop the bubble out of the event list as scaffolding.
func TestAReasoningStringWithNoKnownTextFieldKeepsItsProse(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nwhy is the loader bounded\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Checking the manifest bounds."}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"b2","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"why is the loader bounded",
				"createdAt":"2026-08-18T12:41:29.000Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":b2",
			value: `{"bubbleId":"b2","type":2,"capabilityType":30,"serverBubbleId":"srv-91",
				"createdAt":"2026-08-18T12:41:29.766Z",
				"thinking":"{\"reasoning\":\"Checking the manifest bounds.\",\"isLastThinkingChunk\":true}"}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "b2" {
		t.Fatalf("the reasoning block joined to %v, want the bubble that carried it", e)
	}
}

// A text block gets no positional look-behind: prose must go without provenance rather than
// borrow a neighbour's. An empty block is where the lenient rule would otherwise take anything.
func TestATextBlockDoesNotClaimAPassedOverBubbleOnPositionAlone(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nwhat changed\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}},{"type":"text","text":""}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"prose","type":2},
					{"bubbleId":"read","type":2}]}`, conv),
		},
		{
			key: "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"what changed",
				"createdAt":"2026-08-18T13:53:00.000Z"}`,
		},
		{
			// Prose of its own, which the empty block does not contradict and must not
			// be allowed to consume.
			key: "bubbleId:" + conv + ":prose",
			value: `{"bubbleId":"prose","type":2,"text":"Reading the resolver first.",
				"createdAt":"2026-08-18T13:53:01.000Z"}`,
		},
		{
			key: "bubbleId:" + conv + ":read",
			value: `{"bubbleId":"read","type":2,"capabilityType":15,
				"createdAt":"2026-08-18T13:53:02.000Z",
				"toolFormerData":{"toolCallId":"call_read","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	if e, _ := enrich[0].(map[string]any); e == nil || e["bubbleId"] != "read" {
		t.Errorf("the read joined to %v, want its own bubble", e)
	}
	// The prose carries nothing rather than the bubble the cursor walked past.
	if enrich[1] != nil {
		t.Errorf("the text block was given %v on position alone", enrich[1])
	}
}

// Cursor rewrites a command between the transcript and the store, splicing in its attribution.
// Unstripped, the executed form scores as contradiction on the call's own bubble.
func TestAVendorRewrittenCommandStillAligns(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncommit and open a PR\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git commit -m \"fix: bound the loader\"","description":"Commit the fix"}},{"type":"tool_use","name":"Shell","input":{"command":"gh pr create --title \"Bound the loader\" --body \"$(cat <<'EOB'\n## Summary\n- bound it\nEOB\n)\"","description":"Open the PR"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"commit","type":2},
					{"bubbleId":"pr","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"commit and open a PR"}`,
		},
		{
			// What ran: the commit with the trailer spliced in.
			key: "bubbleId:" + conv + ":commit",
			value: `{"bubbleId":"commit","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_commit","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"",
					"params":"{\"command\":\"git commit --trailer \\\"Co-authored-by: Cursor <cursoragent@cursor.com>\\\" -m \\\"fix: bound the loader\\\"\"}",
					"result":"1 file changed"}}`,
		},
		{
			// What ran: the PR body with the footer appended inside the heredoc.
			key: "bubbleId:" + conv + ":pr",
			value: `{"bubbleId":"pr","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_pr","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"",
					"params":"{\"command\":\"gh pr create --title \\\"Bound the loader\\\" --body \\\"$(cat <<'EOB'\\n## Summary\\n- bound it\\n\\nMade with [Cursor](https://cursor.com)\\nEOB\\n)\\\"\"}",
					"result":"https://github.com/org/repo/pull/1"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the rewritten commands mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"commit", "pr"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
}

// An errored call is recorded with no argument values at all, and recording nothing contradicts
// nothing: scored as contradiction, the bubble was barred even from the positional fallback.
func TestAnErroredCallRecordedWithoutArgumentsAligns(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfix the table\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"file_path":"/work/api/AUDIT.md","old_string":"| batch B | open |","new_string":"| batch B | shipped |"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"edit","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"fix the table"}`,
		},
		{
			// The errored edit, exactly as observed: no path, no strings, just flags.
			key: "bubbleId:" + conv + ":edit",
			value: `{"bubbleId":"edit","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_edit","name":"edit_file_v2",
					"status":"error","rawArgs":"",
					"params":"{\"noCodeblock\":true,\"cloudAgentEdit\":false}",
					"result":"the model produced an invalid edit"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the errored call mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "edit" {
		t.Fatalf("the block joined to %v, want the errored bubble", e)
	}
	if e["status"] != "error" {
		t.Errorf("status = %v", e["status"])
	}
}

// The store's terminal params carry a parse tree of the command, and one of its fragments
// equalling an unrelated block's argument let that block steal the terminal bubble. The header
// order and the read's single comparable value are what make the theft happen here if the parse
// tree is allowed to speak.
func TestTheTerminalParseTreeCannotSpeakForAnotherTool(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nsurvey the repo\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"render.yaml"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git show d63a490 -- render.yaml | head -30","description":"Inspect the pin commit"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"shell","type":2},
					{"bubbleId":"read","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"survey the repo"}`,
		},
		{
			// The read's filename as a shell token, the workspace root under the
			// sandbox policy: none of it may serve as argument evidence.
			key: "bubbleId:" + conv + ":shell",
			value: `{"bubbleId":"shell","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_shell","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"",
					"params":"{\"command\":\"git show d63a490 -- render.yaml | head -30\",\"cwd\":\"\",\"parsingResult\":{\"commands\":[{\"words\":[\"git\",\"show\",\"d63a490\",\"render.yaml\",\"head\"]}]},\"requestedSandboxPolicy\":{\"workspace\":\"/work/api\"},\"commandDescription\":\"Inspect the pin commit\"}",
					"result":"render.yaml | 2 +-"}}`,
		},
		{
			key: "bubbleId:" + conv + ":read",
			value: `{"bubbleId":"read","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_read","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"render.yaml\"}",
					"result":"services:\n  - type: web"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the parse tree stole a bubble: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	for i, want := range []string{"read", "shell"} {
		enrich, _ := lines[i+1]["_enrich"].([]any)
		if len(enrich) != 1 {
			t.Fatalf("line %d _enrich = %v", i+1, lines[i+1]["_enrich"])
		}
		e, _ := enrich[0].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("line %d joined to %v, want %s", i+1, e, want)
		}
	}
	// The theft's signature outcome: the shell call demoted to a repeat of nothing. Zero, or
	// the misattribution shipped silently.
	if res.Objects[0].Repeats != 0 {
		t.Errorf("repeats = %d, want 0: the shell call lost its own bubble", res.Objects[0].Repeats)
	}
}

// One shared value among several is corroboration, not identity. The store's header order puts
// the other search first, so a rule letting the shared path speak hands over the wrong bubble.
func TestALoneSharedValueAmongSeveralIsNotIdentity(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncheck the backends\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"StatusConflict|409","path":"/work/api"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"PurgePrefix|VerifyCapabilities","path":"/work/api"}}]}}
`
	db := newStore(t, []storeRow{
		{
			// purge before conflict: the reverse of the transcript's order.
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"purge","type":2},
					{"bubbleId":"conflict","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"check the backends"}`,
		},
		{
			key: "bubbleId:" + conv + ":purge",
			value: `{"bubbleId":"purge","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_purge","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"PurgePrefix|VerifyCapabilities\",\"path\":\"/work/api\"}",
					"result":"purge.go:14"}}`,
		},
		{
			key: "bubbleId:" + conv + ":conflict",
			value: `{"bubbleId":"conflict","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_conflict","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"StatusConflict|409\",\"path\":\"/work/api\"}",
					"result":"s3.go:88"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the out-of-order pair mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	for i, want := range []string{"conflict", "purge"} {
		enrich, _ := lines[i+1]["_enrich"].([]any)
		if len(enrich) != 1 {
			t.Fatalf("line %d _enrich = %v", i+1, lines[i+1]["_enrich"])
		}
		e, _ := enrich[0].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("line %d joined to %v, want %s", i+1, e, want)
		}
	}
}

// The store records one bubble for identical repeated calls — but the same evidence also fits a
// store that recorded both runs, the second argument-less. Each reading makes the other's
// attachment wrong, so the re-run ships undecided: nothing attached, no alarm, and the
// argument-less bubble barred from later fallbacks.
func TestARepeatedCallTheStoreRecordedOnceIsNotAMismatch(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nlook twice\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"PurgePrefix|opts\\.Prefix","path":"/work/api/internal/backends/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Both live in etag.go."}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"grep","type":2},
					{"bubbleId":"argless","type":2},
					{"bubbleId":"purge","type":2},
					{"bubbleId":"a1","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"look twice"}`,
		},
		{
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"quoteETag|normaliseETag\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"etag.go:9"}}`,
		},
		{
			// An argument-less same-named bubble right where a positional fallback would
			// look. The repeat must not take it — its result is some other call's.
			key: "bubbleId:" + conv + ":argless",
			value: `{"bubbleId":"argless","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_argless","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{}","result":"NOT THE REPEAT'S RESULT"}}`,
		},
		{
			// A following sibling of the same tool: the repeat must leave its bubble alone.
			key: "bubbleId:" + conv + ":purge",
			value: `{"bubbleId":"purge","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_purge","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"PurgePrefix|opts\\\\.Prefix\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"purge.go:14"}}`,
		},
		{
			key:   "bubbleId:" + conv + ":a1",
			value: `{"bubbleId":"a1","type":2,"text":"Both live in etag.go."}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the deduped repeat was reported as a mismatch: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)

	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("first run _enrich = %v", lines[1]["_enrich"])
	}
	if e, _ := enrich[0].(map[string]any); e == nil || e["bubbleId"] != "grep" {
		t.Errorf("first run joined to %v", enrich)
	}
	if _, has := lines[2]["_enrich"]; has {
		t.Errorf("the repeat was given enrichment it has no recorded bubble for: %v", lines[2]["_enrich"])
	}
	enrich, _ = lines[3]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("sibling _enrich = %v", lines[3]["_enrich"])
	}
	if e, _ := enrich[0].(map[string]any); e == nil || e["bubbleId"] != "purge" {
		t.Errorf("the sibling search joined to %v, want its own bubble", e)
	}
	enrich, _ = lines[4]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("closing prose _enrich = %v", lines[4]["_enrich"])
	}
	// The argument-less bubble's result must not surface ANYWHERE: not on the re-run, and
	// not on a later call the declined bubble could otherwise fall back to.
	if strings.Contains(string(res.Objects[0].Payload), "NOT THE REPEAT'S RESULT") {
		t.Error("the undecided argument-less bubble's result reached the derived object")
	}
}

// A hole before a repeat is still a hole, but the repeat is not what proves it: it consumes
// nothing, so the next consuming match is what catches the hole. The trailing case — where
// stamping a watermark discards vendor-explained conversations — is asserted in
// TestATrailingRepeatDoesNotTurnInjectedTurnsIntoMismatches.
func TestAHoleBeforeARepeatIsAMismatchNotTail(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nlook around\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"nothing|the|store|knows","path":"/work/api/internal/engine"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"PurgePrefix|opts\\.Prefix","path":"/work/api/internal/backends/s3"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"grep","type":2},
					{"bubbleId":"purge","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"look around"}`,
		},
		{
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"quoteETag|normaliseETag\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"etag.go:9"}}`,
		},
		{
			key: "bubbleId:" + conv + ":purge",
			value: `{"bubbleId":"purge","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_purge","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"PurgePrefix|opts\\\\.Prefix\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"purge.go:14"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 1 {
		t.Fatalf("mismatched = %d, want 1 (the hole sits before a consuming match): %v",
			res.Mismatched, res.Notes)
	}
	if len(res.Objects) != 0 {
		t.Fatalf("the hole rode out on the tail rule: %v", res.Notes)
	}
}

// A bubble consumed by a positional fallback is not proof of a dedup: reading it as one files
// the bubble's real owner as a repeat and ships the fallback's block wearing the wrong result.
// Only an identity-grade consumer can make a later agreeing call a repeat.
func TestAFallbackConsumerDoesNotMakeTheNextCallARepeat(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nscan the backends\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"PurgePrefix|opts","path":"/work/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/s3"}}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Both live in etag.go."}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"etag","type":2},
					{"bubbleId":"a1","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"scan the backends"}`,
		},
		{
			// The only tool bubble. It belongs to the second search, but the first
			// reaches it first and the shared path keeps it un-contradicted.
			key: "bubbleId:" + conv + ":etag",
			value: `{"bubbleId":"etag","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_etag","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"quoteETag|normaliseETag\",\"path\":\"/work/s3\"}",
					"result":"etag.go:9"}}`,
		},
		{
			key:   "bubbleId:" + conv + ":a1",
			value: `{"bubbleId":"a1","type":2,"text":"Both live in etag.go."}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 1 {
		t.Fatalf("mismatched = %d, want 1 (a fallback consumer must not certify a repeat): %v",
			res.Mismatched, append(res.Notes, res.Infos...))
	}
	if len(res.Objects) != 0 {
		t.Fatalf("shipped a derived object built on a fallback-certified repeat: %v", res.Infos)
	}
}

// Two agreeing values do not outvote a third that actively disagrees: under a two-of-three rule
// these two searches, differing only in pattern, swapped ids, statuses and results with no alarm.
func TestTwoCallsSharingAllButOneArgumentDoNotSwapBubbles(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncheck the goldens\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"ParseManifest|writeManifest","path":"/work/api/handlers","glob":"*.golden"}},{"type":"tool_use","name":"Grep","input":{"pattern":"VerifyPayload|checkPayload","path":"/work/api/handlers","glob":"*.golden"}}]}}
`
	db := newStore(t, []storeRow{
		{
			// The reverse of the transcript's block order within the turn.
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"gb","type":2},
					{"bubbleId":"ga","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"check the goldens"}`,
		},
		{
			key: "bubbleId:" + conv + ":gb",
			value: `{"bubbleId":"gb","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_gb","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"VerifyPayload|checkPayload\",\"path\":\"/work/api/handlers\",\"glob\":\"*.golden\"}",
					"result":"payload.go:70"}}`,
		},
		{
			key: "bubbleId:" + conv + ":ga",
			value: `{"bubbleId":"ga","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_ga","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"ParseManifest|writeManifest\",\"path\":\"/work/api/handlers\",\"glob\":\"*.golden\"}",
					"result":"manifest.go:12"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"ga", "gb"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s (its own bubble, not its sibling's)", i, e, want)
		}
	}
}

// A bubble agreeing on a value too long to be a coincidence outranks one the store recorded
// nothing for. Here an orphaned errored task_v2 sits ahead of the call's own bubble, and
// first-neutral selection shipped the orphan's error status on a successful call.
func TestAPartiallyAgreeingOwnBubbleOutranksAnEvidenceFreeOrphan(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncheck types\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Task","input":{"prompt":"Run npx tsc --noEmit in the repo and report every error.","subagent_type":"general-purpose"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"orphan","type":2},
					{"bubbleId":"task","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"check types"}`,
		},
		{
			key: "bubbleId:" + conv + ":orphan",
			value: `{"bubbleId":"orphan","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"task_orphan","name":"task_v2","tool":38,
					"status":"error","rawArgs":"{}",
					"result":"NOT THIS TASK'S RESULT"}}`,
		},
		{
			key: "bubbleId:" + conv + ":task",
			value: `{"bubbleId":"task","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"task_real","name":"task_v2","tool":38,
					"status":"completed",
					"params":"{\"prompt\":\"Run npx tsc --noEmit in the repo and report every error.\",\"subagentType\":\"SUBAGENT_EXECUTION_ENVIRONMENT_UNSPECIFIED\"}",
					"result":"no type errors"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "task" {
		t.Fatalf("the task joined to %v, want its own prompt-matching bubble", e)
	}
	if e["status"] != "completed" || e["result"] != "no type errors" {
		t.Errorf("the task wears the orphan's outcome: status=%v result=%v", e["status"], e["result"])
	}
}

// An argument shorter than minArgLen cannot confirm identity but can still deny it: with
// three-character patterns invisible to the comparison, the shared path alone certified a swap.
func TestAShortDistinguishingArgumentStillSeparatesTwoCalls(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nsweep the api\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"err","path":"/work/api"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"StatusConflict|409","path":"/work/api"}}]}}
`
	db := newStore(t, []storeRow{
		{
			// Reversed: the conflict search's bubble sits where the err search looks first.
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"conflict","type":2},
					{"bubbleId":"errown","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"sweep the api"}`,
		},
		{
			key: "bubbleId:" + conv + ":conflict",
			value: `{"bubbleId":"conflict","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_conflict","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"StatusConflict|409\",\"path\":\"/work/api\"}",
					"result":"s3.go:88"}}`,
		},
		{
			key: "bubbleId:" + conv + ":errown",
			value: `{"bubbleId":"errown","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_err","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"err\",\"path\":\"/work/api\"}",
					"result":"everywhere.go:1"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	for i, want := range []string{"errown", "conflict"} {
		enrich, _ := lines[i+1]["_enrich"].([]any)
		if len(enrich) != 1 {
			t.Fatalf("line %d _enrich = %v", i+1, lines[i+1]["_enrich"])
		}
		e, _ := enrich[0].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("line %d joined to %v, want %s", i+1, e, want)
		}
	}
}

// The numeric twin: numbers compare as parsed values, so the store's 1 and a transcript 1.0
// agree, and a disagreeing offset vetoes identity even though it can confirm nothing.
func TestANumericArgumentVetoesABorrowedIdentity(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nread both halves\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/engine/engine.go","offset":400}},{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/engine/engine.go","offset":0}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"r0","type":2},
					{"bubbleId":"r400","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"read both halves"}`,
		},
		{
			key: "bubbleId:" + conv + ":r0",
			value: `{"bubbleId":"r0","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_r0","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/engine/engine.go\",\"offset\":0}",
					"result":"the head of the file"}}`,
		},
		{
			key: "bubbleId:" + conv + ":r400",
			value: `{"bubbleId":"r400","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_r400","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/engine/engine.go\",\"offset\":400}",
					"result":"the tail of the file"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"r400", "r0"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
}

// The undecided bubble must not drift to an unrelated later call: left unconsumed, the next
// terminal command took it as its positional fallback and shipped wearing the re-run's result.
func TestAnUndecidableRepeatAttachesNothingAndDoesNotCascade(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nrun the tests\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"npm test"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"npm test"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git status --short"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"t1","type":2},
					{"bubbleId":"t2","type":2},
					{"bubbleId":"t3","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"run the tests"}`,
		},
		{
			key: "bubbleId:" + conv + ":t1",
			value: `{"bubbleId":"t1","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_t1","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{\"command\":\"npm test\"}",
					"result":"FAIL: 3 failing"}}`,
		},
		{
			// The re-run's bubble, recorded argument-less — indistinguishable from a
			// neighbour's argument-less bubble when the run before it was deduped.
			key: "bubbleId:" + conv + ":t2",
			value: `{"bubbleId":"t2","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_t2","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{}",
					"result":"PASS all tests passed"}}`,
		},
		{
			key: "bubbleId:" + conv + ":t3",
			value: `{"bubbleId":"t3","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_t3","name":"run_terminal_command_v2",
					"status":"completed","rawArgs":"{}",
					"result":" M internal/config/resolve.go"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the undecidable re-run was reported as a mismatch: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)

	// Under one reading t2 is the re-run's own bubble, under the other a different call's,
	// and no local signal picks between them.
	if _, has := lines[2]["_enrich"]; has {
		t.Errorf("the undecidable re-run was given enrichment: %v", lines[2]["_enrich"])
	}
	// And the undecided bubble does not cascade: git status gets ITS OWN bubble, not the
	// re-run's declined one.
	enrich, _ := lines[3]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("git status _enrich = %v", lines[3]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "t3" {
		t.Fatalf("git status joined to %v, want t3", e)
	}
	if e["result"] != " M internal/config/resolve.go" {
		t.Errorf("git status result = %v", e["result"])
	}
	// The re-run's likely result must not surface on ANY block.
	if strings.Contains(string(res.Objects[0].Payload), "PASS all tests passed") {
		t.Error("the declined bubble's result reached the derived object on some other block")
	}
}

// Repeat detection is not bounded by the look-behind window: it consumes nothing, so the
// misplacement the window guards against cannot happen. Bounded, one filler call decided whether
// the same shape was a benign repeat or a discarded conversation.
func TestAStoreDedupedReRunFarBackIsStillARepeat(t *testing.T) {
	var blocks []string
	blocks = append(blocks, `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\naudit everything\n</user_query>"}]}}`)
	blocks = append(blocks, `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}`)
	headers := []string{`{"bubbleId":"b1","type":1}`, `{"bubbleId":"grep","type":2}`}
	rows := []storeRow{
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"audit everything"}`,
		},
		{
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"quoteETag|normaliseETag\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"etag.go:9"}}`,
		},
	}
	// 64 distinct consumed bubbles between the original and its re-run: one past the
	// look-behind bound.
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("fill%02d", i)
		blocks = append(blocks, fmt.Sprintf(`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"sym%02dAlpha|sym%02dBeta","path":"/work/api/pkg%02d"}}]}}`, i, i, i))
		headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":2}`, id))
		rows = append(rows, storeRow{
			key: "bubbleId:" + conv + ":" + id,
			value: fmt.Sprintf(`{"bubbleId":%q,"type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_%s","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"sym%02dAlpha|sym%02dBeta\",\"path\":\"/work/api/pkg%02d\"}",
					"result":"pkg%02d.go:1"}}`, id, id, i, i, i, i),
		})
	}
	// The re-run the store deduped, then real prose with a real bubble, so the re-run
	// cannot ride out on the tail rule.
	blocks = append(blocks, `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}`)
	blocks = append(blocks, `{"role":"assistant","message":{"content":[{"type":"text","text":"All checks passed."}]}}`)
	headers = append(headers, `{"bubbleId":"a1","type":2}`)
	rows = append(rows,
		storeRow{key: "bubbleId:" + conv + ":a1", value: `{"bubbleId":"a1","type":2,"text":"All checks passed."}`},
		storeRow{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[%s]}`, conv, strings.Join(headers, ",")),
		})

	res := run(t, newStore(t, rows), unit(t, strings.Join(blocks, "\n")+"\n"))
	if res.Mismatched != 0 {
		t.Fatalf("the far-back dedup was reported as a mismatch: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	if joined := strings.Join(res.Infos, " "); !strings.Contains(joined, "repeat") {
		t.Errorf("the far-back dedup produced no repeat info: %v", res.Infos)
	}
}

// A repeat verdict must survey the whole store first: when the re-run's own bubble exists but
// sits past the forward window, the store did record the run and the loud outcome stands.
func TestARepeatWhoseOwnBubbleIsOutOfReachIsAMismatchNotARepeat(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncheck the resolver twice\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/config/resolve.go"}}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"The resolver is bounded."}]}}
`
	headers := []string{
		`{"bubbleId":"b1","type":1}`,
		`{"bubbleId":"r1","type":2}`,
		`{"bubbleId":"prose","type":2}`,
	}
	rows := []storeRow{
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"check the resolver twice"}`,
		},
		{
			key: "bubbleId:" + conv + ":r1",
			value: `{"bubbleId":"r1","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_r1","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config"}}`,
		},
		{
			key:   "bubbleId:" + conv + ":prose",
			value: `{"bubbleId":"prose","type":2,"text":"The resolver is bounded."}`,
		},
	}
	// Store-only bubbles a subagent left behind: the transcript never mentions them, and
	// they push the re-run's own bubble past the forward window.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("sub%02d", i)
		headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":2}`, id))
		rows = append(rows, storeRow{
			key: "bubbleId:" + conv + ":" + id,
			value: fmt.Sprintf(`{"bubbleId":%q,"type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_%s","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/sub/f%02d.go\"}",
					"result":"subagent detail"}}`, id, id, i),
		})
	}
	headers = append(headers, `{"bubbleId":"r2","type":2}`)
	rows = append(rows,
		storeRow{
			key: "bubbleId:" + conv + ":r2",
			value: `{"bubbleId":"r2","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_r2","name":"read_file_v2",
					"status":"completed","rawArgs":"{\"path\":\"/work/api/internal/config/resolve.go\"}",
					"result":"package config, again"}}`,
		},
		storeRow{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[%s]}`, conv, strings.Join(headers, ",")),
		})

	res := run(t, newStore(t, rows), unit(t, transcript))
	if res.Mismatched != 1 {
		t.Fatalf("mismatched = %d, want 1 (the re-run's own bubble exists, only out of reach): %v",
			res.Mismatched, append(res.Notes, res.Infos...))
	}
	if len(res.Objects) != 0 {
		t.Fatalf("shipped an object claiming a dedup the store contradicts: %v", res.Infos)
	}
}

// A trailing repeat proves nothing about the events before it: Cursor's injected notification
// turns get no bubble, so stamping a watermark there turns a fully explained tail into a mismatch.
func TestATrailingRepeatDoesNotTurnInjectedTurnsIntoMismatches(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfind the etag helpers\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"type":"turn_ended","status":"success"}
{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:05 AM (UTC+2)</timestamp>\n\n<user_query>Briefly inform the user about the task result.</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}}]}}
{"type":"turn_ended","status":"success"}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"u1","type":1},
					{"bubbleId":"grep","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":u1",
			value: `{"bubbleId":"u1","type":1,"text":"find the etag helpers"}`,
		},
		{
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"quoteETag|normaliseETag\",\"path\":\"/work/api/internal/backends/s3\"}",
					"result":"etag.go:9"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("a vendor-injected turn before a trailing repeat became a mismatch: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	joined := strings.Join(res.Infos, " ")
	if !strings.Contains(joined, "extend past the store") {
		t.Errorf("the injected turn produced no tail info: %v", res.Infos)
	}
	if !strings.Contains(joined, "repeat") {
		t.Errorf("the trailing repeat produced no repeat info: %v", res.Infos)
	}
}

// Argument evidence reaches nested inputs: comparing only top-level strings left such a tool
// permanently neutral, so under header reorder two calls of it swapped bubbles on position alone.
func TestNestedInputValuesAreArgumentEvidence(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\napply both edits\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"MultiEdit","input":{"edits":[{"file_path":"/work/api/internal/config/resolve.go","old_string":"the bound is unchecked here","new_string":"the bound is enforced here"}]}},{"type":"tool_use","name":"MultiEdit","input":{"edits":[{"file_path":"/work/api/internal/config/load.go","old_string":"loads the whole file eagerly","new_string":"streams the file in pages"}]}}]}}
`
	db := newStore(t, []storeRow{
		{
			// Reversed relative to the transcript's block order.
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"e2","type":2},
					{"bubbleId":"e1","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"apply both edits"}`,
		},
		{
			key: "bubbleId:" + conv + ":e2",
			value: `{"bubbleId":"e2","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_e2","name":"multi_edit_v2",
					"status":"completed","rawArgs":"{\"edits\":[{\"file_path\":\"/work/api/internal/config/load.go\",\"old_string\":\"loads the whole file eagerly\",\"new_string\":\"streams the file in pages\"}]}",
					"result":"1 edit applied to load.go"}}`,
		},
		{
			key: "bubbleId:" + conv + ":e1",
			value: `{"bubbleId":"e1","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_e1","name":"multi_edit_v2",
					"status":"completed","rawArgs":"{\"edits\":[{\"file_path\":\"/work/api/internal/config/resolve.go\",\"old_string\":\"the bound is unchecked here\",\"new_string\":\"the bound is enforced here\"}]}",
					"result":"1 edit applied to resolve.go"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 2 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	for i, want := range []string{"e1", "e2"} {
		e, _ := enrich[i].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("block %d joined to %v, want %s", i, e, want)
		}
	}
}

// The containment path — for stores that recorded arguments as a bare string — strips the
// attribution rewrite like every other comparison, in both the plain and the escaped form.
func TestABareStringStoreWithAttributionStillAligns(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncommit and open a PR\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git commit -m \"fix: bound the loader in the manifest reader\""}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"gh pr create --title \"Bound the loader\" --body \"## Summary\n- bound it\""}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"commit","type":2},
					{"bubbleId":"pr","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"commit and open a PR"}`,
		},
		{
			// A bare string, not JSON: the executed command with the trailer spliced in.
			key: "bubbleId:" + conv + ":commit",
			value: `{"bubbleId":"commit","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_commit","name":"run_terminal_command_v2",
					"status":"completed",
					"rawArgs":"git commit --trailer \"Co-authored-by: Cursor <cursoragent@cursor.com>\" -m \"fix: bound the loader in the manifest reader\"",
					"result":"1 file changed"}}`,
		},
		{
			// A bare string holding JSON: the executed command appears escaped, footer
			// included, so the escaped attribution form is the one that must strip.
			key: "bubbleId:" + conv + ":pr",
			value: `{"bubbleId":"pr","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_pr","name":"run_terminal_command_v2",
					"status":"completed",
					"rawArgs":"captured {\"command\":\"gh pr create --title \\\"Bound the loader\\\" --body \\\"## Summary\\n- bound it\\n\\nMade with [Cursor](https://cursor.com)\\\"\"} (exit 0)",
					"result":"https://github.com/org/repo/pull/1"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if res.Mismatched != 0 {
		t.Fatalf("the rewritten bare-string commands mismatched: %v", res.Notes)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("no derived object: %v", res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	for i, want := range []string{"commit", "pr"} {
		enrich, _ := lines[i+1]["_enrich"].([]any)
		if len(enrich) != 1 {
			t.Fatalf("line %d _enrich = %v", i+1, lines[i+1]["_enrich"])
		}
		e, _ := enrich[0].(map[string]any)
		if e == nil || e["bubbleId"] != want {
			t.Errorf("line %d joined to %v, want %s", i+1, e, want)
		}
	}
}

// When two bubbles both agree in full, the one agreeing on more of the call is the call:
// first-positive-wins handed a search to a terminal record that merely quoted its path.
func TestTheStrongerOfTwoAgreeingBubblesWins(t *testing.T) {
	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nfind the decompression bound\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"maxDecompressedBytes|decompressLimitCeiling","path":"/work/api/internal/backends/s3/multipart"}}]}}
`
	db := newStore(t, []storeRow{
		{
			key: "composerData:" + conv,
			value: fmt.Sprintf(`{"composerId":%q,"createdAt":1755500000000,
				"fullConversationHeadersOnly":[
					{"bubbleId":"b1","type":1},
					{"bubbleId":"ls","type":2},
					{"bubbleId":"grep","type":2}]}`, conv),
		},
		{
			key:   "bubbleId:" + conv + ":b1",
			value: `{"bubbleId":"b1","type":1,"text":"find the decompression bound"}`,
		},
		{
			// An older-generation bare-string record of a DIFFERENT call that quotes
			// the search's path: containment confirms it on that one value.
			key: "bubbleId:" + conv + ":ls",
			value: `{"bubbleId":"ls","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_ls","name":"run_terminal_command_v2",
					"status":"completed",
					"rawArgs":"ls -la /work/api/internal/backends/s3/multipart",
					"result":"NOT THE SEARCH'S RESULT"}}`,
		},
		{
			key: "bubbleId:" + conv + ":grep",
			value: `{"bubbleId":"grep","type":2,"capabilityType":15,
				"toolFormerData":{"toolCallId":"call_grep","name":"ripgrep_raw_search",
					"status":"completed","rawArgs":"{\"pattern\":\"maxDecompressedBytes|decompressLimitCeiling\",\"path\":\"/work/api/internal/backends/s3/multipart\"}",
					"result":"multipart.go:88"}}`,
		},
	})

	res := run(t, db, unit(t, transcript))
	if len(res.Objects) != 1 {
		t.Fatalf("objects = %d, mismatched %d, notes %v", len(res.Objects), res.Mismatched, res.Notes)
	}
	lines := decode(t, res.Objects[0].Payload)
	enrich, _ := lines[1]["_enrich"].([]any)
	if len(enrich) != 1 {
		t.Fatalf("_enrich = %v", lines[1]["_enrich"])
	}
	e, _ := enrich[0].(map[string]any)
	if e == nil || e["bubbleId"] != "grep" {
		t.Fatalf("the search joined to %v, want the bubble agreeing on both values", e)
	}
}
