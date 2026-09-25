package cursorjoin_test

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// fullRows is the happy path: headers name every bubble, the tool bubble carries the result.
func fullRows() []storeRow {
	return conversation(
		bubbleRow("b1", `{"type":1,"text":"list the workspace","createdAt":"2026-07-28T10:06:00.000Z","requestId":"req-1"}`),
		bubbleRow("b2", `{"type":2,"text":"Listing the workspace folder contents.","createdAt":"2026-07-28T10:06:01.000Z",
			"requestId":"req-2","modelName":"claude-4.5-sonnet","turnDurationMs":1420,"checkpointId":"ckpt-9"}`),
		lsBubble(`"createdAt":"2026-07-28T10:06:02.000Z","toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
			"status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
			"result":"total 24\ndrwxr-xr-x  5 jane staff  160 Jul 28 10:05 .\n-rw-r--r--  1 jane staff  812 Jul 28 09:58 main.go"}`),
	)
}

// lsRows is transcript's store with the fields of its tool bubble b3 supplied by the caller.
func lsRows(toolFields string) []storeRow {
	return conversation(user("b1", "list the workspace"), said("b2", "Listing the workspace folder contents."), lsBubble(toolFields))
}

// lsTool is the plainest tool bubble transcript's ls call aligns to.
const lsTool = `"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}`

func lsBubble(fields string) storeRow { return bubbleRow("b3", `{"type":2,`+fields+`}`) }

// conversation is a composer whose header order is the order of the bubbles, then the bubbles.
func conversation(bubbles ...storeRow) []storeRow {
	headers := make([]string, len(bubbles))
	for i, b := range bubbles {
		var body struct{ Type int }
		_ = json.Unmarshal([]byte(b.value), &body)
		headers[i] = fmt.Sprintf(`{"bubbleId":%q,"type":%d}`, b.key[strings.LastIndex(b.key, ":")+1:], body.Type)
	}
	composer := fmt.Sprintf(`{"composerId":%q,"createdAt":1753700000000,"fullConversationHeadersOnly":[%s]}`, conv, strings.Join(headers, ","))
	return append([]storeRow{{"composerData:" + conv, composer}}, bubbles...)
}

// bubbleRow stores body under id; the join takes the bubble id from the key.
func bubbleRow(id, body string) storeRow { return storeRow{"bubbleId:" + conv + ":" + id, body} }

func user(id, text string) storeRow { return prose(id, 1, text) }
func said(id, text string) storeRow { return prose(id, 2, text) }

func prose(id string, typ int, text string) storeRow {
	body, _ := json.Marshal(map[string]any{"type": typ, "text": text})
	return bubbleRow(id, string(body))
}

// tool is a tool bubble in Cursor's standard envelope; status defaults to completed, and kind is the
// numeric tool enum current stores write.
type tool struct {
	id, name, rawArgs, params, status, result string
	kind                                      int
}

func (tl tool) row() storeRow {
	data := map[string]any{"toolCallId": "call_" + tl.id, "name": tl.name, "status": cmp.Or(tl.status, "completed"),
		"rawArgs": tl.rawArgs, "params": tl.params, "result": tl.result}
	if tl.kind != 0 {
		data["tool"] = tl.kind
	}
	body, _ := json.Marshal(map[string]any{"type": 2, "capabilityType": 15, "toolFormerData": data})
	return bubbleRow(tl.id, string(body))
}

func toolRow(id, name, rawArgs, result string) storeRow {
	return tool{id: id, name: name, rawArgs: rawArgs, result: result}.row()
}

// matchedBubbles asserts the bubble each block of line joined to, "" for a block with none.
func matchedBubbles(t *testing.T, line map[string]any, ids ...string) []map[string]any {
	t.Helper()
	if len(ids) == 0 {
		require.NotContains(t, line, "_enrich", "a line with no joined block carries no enrichment field")
		return nil
	}
	items, ok := line["_enrich"].([]any)
	require.Truef(t, ok, "_enrich = %v", line["_enrich"])
	require.Len(t, items, len(ids))
	out := make([]map[string]any, len(items))
	for i, item := range items {
		out[i], _ = item.(map[string]any)
		if ids[i] == "" {
			assert.Nil(t, item, "block %d", i)
			continue
		}
		require.NotNil(t, out[i], "block %d", i)
		assert.Equal(t, ids[i], out[i]["bubbleId"], "block %d", i)
	}
	return out
}

func successfulObject(t *testing.T, res transforms.EnrichResult) transforms.Derived {
	t.Helper()
	require.Zerof(t, res.Mismatched, "mismatch notes: %v", res.Notes)
	require.Lenf(t, res.Objects, 1, "derived object missing: %v", res.Notes)
	return res.Objects[0]
}

// joinCase is one conversation; a query prepends that turn and bubble b1, and rows replace the bubbles.
type joinCase struct {
	name       string
	query      string
	transcript string // JSONL; the shared transcript when empty
	bubbles    []storeRow
	rows       []storeRow
	want       map[int][]string // transcript line -> the bubble each block joined to, "" for none
	present    []string         // must appear in the derived object
	absent     []string         // must appear nowhere in the derived object
	infos      []string         // must appear in the object's infos
	mismatch   bool             // no object, one mismatch alarm
	check      func(t *testing.T, d transforms.Derived, lines []map[string]any)
}

func runJoinCases(t *testing.T, cases []joinCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := cmp.Or(tc.transcript, transcript)
			if tc.query != "" {
				body = `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n` + tc.query + `\n</user_query>"}]}}` + "\n" + body
				tc.bubbles = append([]storeRow{user("b1", tc.query)}, tc.bubbles...)
			}
			if tc.rows == nil {
				tc.rows = conversation(tc.bubbles...)
			}
			res := run(t, newStore(t, tc.rows), unit(t, strings.TrimRight(body, "\n")+"\n"))
			if tc.mismatch {
				require.Equalf(t, 1, res.Mismatched, "notes: %v", res.Notes)
				require.Emptyf(t, res.Objects, "infos: %v", res.Infos)
				assert.Contains(t, strings.Join(res.Notes, " "), "raw transcript ships", "the alarm must say the raw file is unaffected")
				return
			}
			require.NotContains(t, strings.Join(res.Notes, " "), "did not decode")
			d := successfulObject(t, res)
			lines := decode(t, d.Payload)
			require.Len(t, lines, strings.Count(strings.TrimRight(body, "\n"), "\n")+1, "every native line survives")
			for i, ids := range tc.want {
				matchedBubbles(t, lines[i], ids...)
			}
			for _, s := range tc.present {
				assert.Contains(t, string(d.Payload), s)
			}
			for _, s := range tc.absent {
				assert.NotContains(t, string(d.Payload), s)
			}
			for _, s := range tc.infos {
				assert.Contains(t, strings.Join(res.Infos, " "), s)
			}
			if tc.check != nil {
				tc.check(t, d, lines)
			}
		})
	}
}

func jsonl(lines ...string) string { return strings.Join(lines, "\n") }

// turn is one assistant transcript line holding the given blocks.
func turn(blocks ...string) string {
	return `{"role":"assistant","message":{"content":[` + strings.Join(blocks, ",") + `]}}`
}

func use(name, input string) string {
	return `{"type":"tool_use","name":"` + name + `","input":` + input + `}`
}

func text(s string) string {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
	return string(b)
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

// THE JOIN: what the transcript cannot say, beside the native lines, with provenance, deterministically.
func TestTheJoinCarriesTheFieldsTheTranscriptLacks(t *testing.T) {
	d := successfulObject(t, run(t, newStore(t, fullRows()), unit(t, transcript)))
	require.Equal(t, transforms.StatusOK, d.Status)
	assert.Truef(t, strings.HasSuffix(d.NativePath, conv+".jsonl.enriched.jsonl"), "derived path = %s", d.NativePath)

	lines := decode(t, d.Payload)
	require.Len(t, lines, 3, "one derived line per transcript line")
	tool := matchedBubbles(t, lines[1], "b2", "b3")[1]
	assert.Equal(t, "call_abc123", tool["tool_call_id"])
	assert.Equal(t, "completed", tool["status"])
	assert.Equal(t, "run_terminal_cmd", tool["tool_name"], "the INTERNAL name, not the transcript's display name")
	assert.Contains(t, tool["result"], "main.go")
	// A real timestamp, where the transcript's only time signal is prose in a user message.
	assert.Equal(t, "2026-07-28T10:06:02.000Z", tool["createdAt"])

	// Byte-identical, not merely equivalent: downstream verifies the derived object against the raw file.
	want := strings.Split(strings.TrimRight(transcript, "\n"), "\n")
	for i, line := range strings.Split(strings.TrimRight(string(d.Payload), "\n"), "\n") {
		var m struct{ Native json.RawMessage }
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		assert.Equalf(t, want[i], string(m.Native), "line %d was re-encoded", i)
	}

	// Without the pairing, a derived object is an assertion nobody can check.
	assert.Equal(t, []string{unit(t, transcript).SourceHash}, d.DerivedFrom)
	assert.NotEmpty(t, d.DBReadMethod)
	assert.NotZero(t, d.DBRowsRead)
	assert.Equal(t, []string{"composerData:", "bubbleId:"}, d.DBKeyspaces, "provenance records the declared scope")

	// Same input values, same output bytes, or the hash stops being a change signal.
	again := successfulObject(t, run(t, newStore(t, fullRows()), unit(t, transcript)))
	assert.Equal(t, string(d.Payload), string(again.Payload))
	assert.Equal(t, d.OutputHash, again.OutputHash)

	// A DB-side-only update: the transcript is byte-identical, so only the output hash can signal it.
	after := successfulObject(t, run(t, newStore(t, lsRows(`"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
		"status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"total 24\nA LATE RESULT ARRIVED"}`)), unit(t, transcript)))
	assert.NotEqual(t, d.OutputHash, after.OutputHash, "a DB-side-only change would never ship")
	assert.Contains(t, string(after.Payload), "A LATE RESULT ARRIVED")
}

// Stores that account for less than the transcript, or more than the join reads.
func TestPartialStores(t *testing.T) {
	runJoinCases(t, []joinCase{{
		// A store describing a different conversation cannot pass as a tail.
		name:     "an unaligned transcript is an alarm, not an object",
		rows:     conversation(user("b1", "something else entirely"), lsBubble(`"toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"/etc/hosts\"}"}`)),
		mismatch: true,
	}, {
		// THE COMPACTION FIXTURE: headers still name bubbles whose rows are gone.
		name: "a compacted conversation still enriches",
		rows: append([]storeRow{{"composerData:" + conv, `{"summarizedComposers":[{"summary":"earlier turns were compacted away"}],
			"fullConversationHeadersOnly":[{"bubbleId":"gone1"},{"bubbleId":"gone2"},{"bubbleId":"b1"},{"bubbleId":"b2"},{"bubbleId":"b3"}]}`}},
			lsRows(lsTool)[1:]...),
		want: map[int][]string{1: {"b2", "b3"}},
	}, {
		// Neither has a transcript counterpart; left in the event list they shift every alignment.
		name: "scaffolding and reasoning bubbles are not events",
		rows: conversation(
			bubbleRow("cap", `{"type":2,"isCapabilityIteration":true,"capabilityType":"tool-negotiation"}`),
			user("b1", "list the workspace"),
			bubbleRow("think", `{"type":2,"isThought":true,"text":"internal reasoning"}`),
			said("b2", "Listing the workspace folder contents."),
			lsBubble(lsTool)),
		want:   map[int][]string{1: {"b2", "b3"}},
		absent: []string{"tool-negotiation", "internal reasoning"},
	}, {
		// An empty header list, as on errored turns: key order would put aaa, mmm, zzz.
		name: "the bubble scan fallback orders by createdAt",
		rows: append(conversation(),
			bubbleRow("zzz", `{"type":2,"text":"Listing the workspace folder contents.","createdAt":"2026-07-28T10:06:01Z"}`),
			bubbleRow("aaa", `{"type":1,"text":"list the workspace","createdAt":"2026-07-28T10:06:00Z"}`),
			bubbleRow("mmm", `{"type":2,"createdAt":"2026-07-28T10:06:02Z",`+lsTool+`}`)),
		want: map[int][]string{0: {"aaa"}, 1: {"zzz", "mmm"}},
	}, {
		// Writes are not atomic: complete lines enrich, and the torn fragment is kept as a string.
		name:       "a truncated transcript tail still enriches what came before",
		transcript: transcript + `{"role":"assistant","message":{"content":[{"type":"te`,
		rows:       fullRows(),
		want:       map[int][]string{1: {"b2", "b3"}},
		present:    []string{"native_invalid"},
	}, {
		// THE NO-ROWS RULE: the derived object is a join of named fields, not a dump.
		name: "no store row reaches the derived object wholesale",
		rows: []storeRow{
			{"composerData:" + conv, `{"UNUSED_MARKER":"must-not-ship","fullConversationHeadersOnly":[{"bubbleId":"b1"},{"bubbleId":"b2"},{"bubbleId":"b3"}]}`},
			bubbleRow("b1", `{"type":1,"text":"list the workspace","ALSO_UNUSED":"must-not-ship"}`),
			said("b2", "Listing the workspace folder contents."),
			lsBubble(lsTool),
		},
		want:   map[int][]string{1: {"b2", "b3"}},
		absent: []string{"UNUSED_MARKER", "ALSO_UNUSED", "must-not-ship"},
	}})
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

// Vendor drift on either side: a struct rejecting a drifted field would drop the whole row or line.
func TestStoreAndTranscriptGenerations(t *testing.T) {
	const loaderQuery = "why is the loader bounded"
	runJoinCases(t, []joinCase{{
		// Numeric enums, capabilityType 15 on tool bubbles, a thinking object, modelInfo.modelName.
		name: "the current store generation",
		rows: conversation(
			user("b1", "list the workspace"),
			bubbleRow("think1", `{"type":2,"text":"","capabilityType":30,"thinking":{"text":"the user wants a listing","signature":"sig"}}`),
			bubbleRow("a1", `{"type":2,"text":"Listing the workspace folder contents.\n\n\n","modelInfo":{"modelName":"composer-2.5"}}`),
			bubbleRow("tool1", `{"type":2,"text":"","capabilityType":15,"toolFormerData":{"toolCallId":"tool_33e5b6ee",
				"name":"run_terminal_command_v2","tool":15,"status":"completed","rawArgs":"",
				"params":"{\"command\":\"ls -la /work/api\",\"cwd\":\"\"}","result":"{\"output\":\"total 24\\nmain.go\"}"}}`),
		),
		want:    map[int][]string{1: {"a1", "tool1"}},
		present: []string{"composer-2.5"},
		check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
			e := matchedBubbles(t, lines[1], "a1", "tool1")[1]
			assert.Contains(t, e["result"], "main.go")
			assert.Equal(t, "run_terminal_command_v2", e["tool_name"])
			assert.Equal(t, "tool_33e5b6ee", e["tool_call_id"])
		},
		// [REDACTED] reasoning leaves nothing to attach the thinking bubble to.
		absent: []string{"the user wants a listing"},
	}, {
		// Reasoning as text, an argument-less terminal call, and injected turns with no rows (tail).
		name: "the current transcript generation",
		transcript: jsonl(
			`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:00 AM (UTC+2)</timestamp>\n<user_query>\nstart the dev server\n</user_query>"}]}}`,
			turn(text("I need to check if a dev server is already running, then start it."), use("Shell", `{"command":"pnpm dev","description":"Start the dev server"}`)),
			turn(text("The server is up on port 5173.")),
			`{"type":"turn_ended","status":"success"}`,
			`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:05 AM (UTC+2)</timestamp>\n\n<user_query>Briefly inform the user about the task result.</user_query>"}]}}`,
			turn(text("The dev server is running.")),
			`{"type":"turn_ended","status":"success"}`),
		bubbles: []storeRow{
			user("b1", "start the dev server"),
			bubbleRow("think1", `{"type":2,"text":"","capabilityType":30,"thinking":{"text":"I need to check if a dev server is already running, then start it."}}`),
			tool{id: "tool1", name: "run_terminal_command_v2", rawArgs: "{}", result: `{"output":"VITE ready on :5173"}`, kind: 15}.row(),
			said("a1", "The server is up on port 5173."),
		},
		want:    map[int][]string{1: {"think1", "tool1"}, 2: {"a1"}},
		present: []string{"VITE ready"},
		infos:   []string{"extend past the store"},
		check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
			assert.Equal(t, "call_tool1", matchedBubbles(t, lines[1], "think1", "tool1")[1]["tool_call_id"])
		},
	}, {
		// The assistant line's role and the terminator's status arrive as numbers.
		name: "a type-drifted transcript line still decodes",
		transcript: `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nlist the workspace\n</user_query>"}]}}
{"role":2,"message":{"content":[{"type":"text","text":"Listing the workspace folder contents."},{"type":"tool_use","name":"Shell","input":{"command":"ls -la /work/api"}}]}}
{"type":"turn_ended","status":3}`,
		rows:   fullRows(),
		want:   map[int][]string{1: {"b2", "b3"}},
		absent: []string{"native_invalid"},
	}, {
		// capabilityType a bool, tool an object: shapes no store has written yet must not cost the row.
		name: "a flex field with an unexpected shape",
		rows: lsRows(`"capabilityType":true,"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd",
			"tool":{"kind":15},"status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}","result":"ok"}`),
		want:    map[int][]string{1: {"b2", "b3"}},
		present: []string{"call_abc123"},
	}, {
		// Server-hydrated thinking is a string holding the object, locally streamed thinking the object.
		name:       "server-hydrated reasoning is decoded, not dropped",
		query:      loaderQuery,
		transcript: turn(text("Checking where the loader's bounds are set."), text("The bound is the row cap.")),
		bubbles: []storeRow{
			bubbleRow("b2", `{"type":2,"capabilityType":30,"thinking":"{\"text\":\"Checking where the loader's bounds are set.\",\"isLastThinkingChunk\":true}"}`),
			bubbleRow("b3", `{"type":2,"capabilityType":30,"thinking":{"text":"The bound is the row cap.","signature":"sig-abc"},"thinkingStyle":1}`),
		},
		want: map[int][]string{1: {"b2", "b3"}},
	}, {
		// Taking the known text field alone would drop this bubble out of the event list as scaffolding.
		name:       "a reasoning string with no known text field keeps its prose",
		query:      loaderQuery,
		transcript: turn(text("Checking the manifest bounds.")),
		bubbles: []storeRow{bubbleRow("b2", `{"type":2,"capabilityType":30,
			"thinking":"{\"reasoning\":\"Checking the manifest bounds.\",\"isLastThinkingChunk\":true}"}`)},
		want: map[int][]string{1: {"b2"}},
	}})
}

// Where the matcher looks: behind the cursor for out-of-order bubbles, never past the forward window.
func TestMatchWindows(t *testing.T) {
	var far []storeRow
	for i := range 20 {
		far = append(far, toolRow(fmt.Sprintf("fill%02d", i), "read_file_v2", fmt.Sprintf(`{"path":"/work/api/f%02d.go"}`, i), "unrelated"))
	}
	// Twenty unrelated calls put a tempting match beyond the forward window.
	beyondWindow := func(name, input, distantArgs string) joinCase {
		return joinCase{
			name:       name,
			query:      "find the deny rules",
			transcript: turn(use("Grep", input)),
			bubbles: append(append([]storeRow{toolRow("near", "ripgrep_raw_search", `{}`, "the right result")}, far...),
				toolRow("far", "ripgrep_raw_search", distantArgs, "the WRONG result")),
			want:   map[int][]string{1: {"near"}},
			absent: []string{"the WRONG result"},
		}
	}
	const read = `{"path":"/work/api/internal/config/resolve.go"}`
	runJoinCases(t, []joinCase{
		{
			// The store puts the task before the prose, so the prose match advances the cursor past it.
			name:       "an out-of-order tool bubble is found behind the cursor",
			query:      "check types",
			transcript: turn(text("Spawning a typecheck subagent."), use("Task", `{"description":"Typecheck the repo","prompt":"Run npx tsc --noEmit in the repo and report errors."}`)),
			bubbles: []storeRow{
				tool{id: "task1", name: "task_v2", result: "no type errors", kind: 38,
					params: `{"description":"Typecheck the repo","prompt":"Run npx tsc --noEmit in the repo and report errors."}`}.row(),
				said("a1", "Spawning a typecheck subagent."),
			},
			want: map[int][]string{1: {"a1", "task1"}},
			check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
				assert.Equal(t, "call_task1", matchedBubbles(t, lines[1], "a1", "task1")[1]["tool_call_id"])
			},
		},
		beyondWindow("substring coincidence stays out of reach", `{"pattern":"deny","glob":"**/*","output_mode":"files_with_matches"}`,
			`{"pattern":"signed","glob":"**/*.{md,json,yaml,go}"}`),
		{
			// An argument-less call never grades positive: the nearest name-compatible bubble is the last resort.
			name:       "a terminal bubble left behind the cursor is still found",
			query:      "what changed",
			transcript: turn(use("Read", read), use("Shell", `{"command":"git status --short"}`)),
			bubbles: []storeRow{
				toolRow("shell", "run_terminal_command_v2", `{}`, " M internal/config/resolve.go"),
				toolRow("read", "read_file_v2", read, "package config"),
			},
			want: map[int][]string{1: {"read", "shell"}},
			check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
				assert.Equal(t, " M internal/config/resolve.go", matchedBubbles(t, lines[1], "read", "shell")[1]["result"])
			},
		},
		beyondWindow("identical arguments stay out of reach", `{"pattern":"deny_additions","path":"/work/api/internal/config"}`,
			`{"pattern":"deny_additions","path":"/work/api/internal/config"}`),
		{
			// Nothing but position separates two argument-less siblings: the nearest wins.
			name:       "the positional look-behind takes the nearest sibling",
			query:      "what changed",
			transcript: turn(use("Read", read), use("Shell", `{"command":"git status --short"}`)),
			bubbles: []storeRow{
				toolRow("shellFar", "run_terminal_command_v2", `{}`, "the FAR sibling"),
				toolRow("shellNear", "run_terminal_command_v2", `{}`, " M internal/config/resolve.go"),
				toolRow("read", "read_file_v2", read, "package config"),
			},
			want: map[int][]string{1: {"read", "shellNear"}},
		}, {
			// Prose gets no positional look-behind: an empty block would otherwise take anything.
			name:       "a text block does not claim a passed-over bubble on position alone",
			query:      "what changed",
			transcript: turn(use("Read", read), text("")),
			bubbles:    []storeRow{said("prose", "Reading the resolver first."), toolRow("read", "read_file_v2", read, "package config")},
			want:       map[int][]string{1: {"read", ""}},
		},
	})
}

// A row that still fails to decode is counted out loud: silence surfaces only as a mismatch
// alarm pointing at alignment instead.
func TestUndecodableStoreRowsProduceANote(t *testing.T) {
	res := run(t, newStore(t, append(lsRows(lsTool), bubbleRow("bad", `{"bubbleId":{"not":"a string"},"type":2}`))), unit(t, transcript))
	assert.Contains(t, strings.Join(res.Notes, " "), "did not decode")
	successfulObject(t, res)
}

// Only the staged conversations' rows are decoded, so another conversation's drifted row neither
// raises the note nor changes the object.
func TestUndecodableRowsOfUnstagedConversationsAreNotCounted(t *testing.T) {
	const other = "0e9d8c7b-6a5f-4e3d-9c2b-1a0f9e8d7c6b"
	unstaged := []storeRow{
		{"bubbleId:" + other + ":bad", `{"bubbleId":{"not":"a string"},"type":2}`},
		{"bubbleId:" + other, `{}`},
		{"composerData:" + other, `{"fullConversationHeadersOnly":"not a list"}`},
	}
	clean := successfulObject(t, run(t, newStore(t, lsRows(lsTool)), unit(t, transcript)))
	res := run(t, newStore(t, append(lsRows(lsTool), unstaged...)), unit(t, transcript))
	assert.Empty(t, res.Notes)
	d := successfulObject(t, res)
	assert.Equal(t, clean.OutputHash, d.OutputHash)
	assert.Equal(t, clean.DBRowsRead+len(unstaged), d.DBRowsRead, "rows read still counts every row in the keyspaces")

	res = run(t, newStore(t, append(append(lsRows(lsTool), unstaged...), bubbleRow("bad", `{"bubbleId":{"not":"a string"},"type":2}`))), unit(t, transcript))
	assert.Contains(t, res.Notes, "1 store rows of the staged conversations did not decode (the store schema is vendor behaviour and drifts)")
	successfulObject(t, res)
}

// No database is not an error, and an unreadable one fails open with a count.
func TestAMissingOrUnreadableDatabaseFailsOpen(t *testing.T) {
	res := run(t, "", unit(t, transcript))
	assert.Equal(t, transforms.EnrichResult{EnricherID: "cursor-transcript-join", Version: 4, Skipped: 1, Notes: []string{"no state.vscdb found: raw transcripts only"}}, res)

	path := filepath.Join(t.TempDir(), "state.vscdb")
	require.NoError(t, os.WriteFile(path, []byte("this is not a database"), 0o600))
	res = run(t, path, unit(t, transcript))
	assert.Empty(t, res.Objects)
	assert.Equal(t, 1, res.Errors)
	assert.NotEmpty(t, res.Notes, "an unreadable database produced no note")
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

// Tool-specific shapes of the stored arguments: what they must not match, and what they still must.
func TestToolArgumentShapes(t *testing.T) {
	runJoinCases(t, []joinCase{{
		// A directory path is a prefix of every file under it.
		name:       "a directory argument does not match a file beneath it",
		query:      "audit the config package",
		transcript: turn(use("Grep", `{"pattern":"deny_additions","path":"/work/api/internal/config"}`), use("Read", `{"path":"/work/api/internal/config/resolve.go"}`)),
		bubbles: []storeRow{
			toolRow("grep", "ripgrep_raw_search", `{}`, "resolve.go:41"),
			toolRow("read", "read_file_v2", `{"path":"/work/api/internal/config/resolve.go"}`, "package config"),
		},
		want: map[int][]string{1: {"grep", "read"}},
	}, {
		// Cursor splices its attribution into what ran; unstripped, that scores as contradiction.
		name:  "a vendor-rewritten command still aligns",
		query: "commit and open a PR",
		transcript: turn(use("Shell", `{"command":"git commit -m \"fix: bound the loader\"","description":"Commit the fix"}`),
			use("Shell", `{"command":"gh pr create --title \"Bound the loader\" --body \"$(cat <<'EOB'\n## Summary\n- bound it\nEOB\n)\"","description":"Open the PR"}`)),
		bubbles: []storeRow{
			tool{id: "commit", name: "run_terminal_command_v2", result: "1 file changed",
				params: `{"command":"git commit --trailer \"Co-authored-by: Cursor <cursoragent@cursor.com>\" -m \"fix: bound the loader\""}`}.row(),
			tool{id: "pr", name: "run_terminal_command_v2", result: "https://github.com/org/repo/pull/1",
				params: `{"command":"gh pr create --title \"Bound the loader\" --body \"$(cat <<'EOB'\n## Summary\n- bound it\n\nMade with [Cursor](https://cursor.com)\nEOB\n)\""}`}.row(),
		},
		want: map[int][]string{1: {"commit", "pr"}},
	}, {
		// The containment path for bare-string records strips the attribution too, plain and escaped.
		name:  "a bare-string store with attribution still aligns",
		query: "commit and open a PR",
		transcript: jsonl(turn(use("Shell", `{"command":"git commit -m \"fix: bound the loader in the manifest reader\""}`)),
			turn(use("Shell", `{"command":"gh pr create --title \"Bound the loader\" --body \"## Summary\n- bound it\""}`))),
		bubbles: []storeRow{
			toolRow("commit", "run_terminal_command_v2", `git commit --trailer "Co-authored-by: Cursor <cursoragent@cursor.com>" -m "fix: bound the loader in the manifest reader"`, "1 file changed"),
			// A bare string holding JSON: the footer appears escaped.
			toolRow("pr", "run_terminal_command_v2", `captured {"command":"gh pr create --title \"Bound the loader\" --body \"## Summary\n- bound it\n\nMade with [Cursor](https://cursor.com)\""} (exit 0)`, "https://github.com/org/repo/pull/1"),
		},
		want: map[int][]string{1: {"commit"}, 2: {"pr"}},
	}, {
		// An errored call records only flags, and recording nothing contradicts nothing.
		name:       "an errored call recorded without arguments aligns",
		query:      "fix the table",
		transcript: turn(use("StrReplace", `{"file_path":"/work/api/AUDIT.md","old_string":"| batch B | open |","new_string":"| batch B | shipped |"}`)),
		bubbles: []storeRow{tool{id: "edit", name: "edit_file_v2", status: "error",
			params: `{"noCodeblock":true,"cloudAgentEdit":false}`, result: "the model produced an invalid edit"}.row()},
		want: map[int][]string{1: {"edit"}},
		check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
			assert.Equal(t, "error", matchedBubbles(t, lines[1], "edit")[0]["status"])
		},
	}, {
		// A fragment of the terminal parse tree equal to the read's only value must not steal the bubble.
		name:  "the terminal parse tree cannot speak for another tool",
		query: "survey the repo",
		transcript: jsonl(turn(use("Read", `{"file_path":"render.yaml"}`)),
			turn(use("Shell", `{"command":"git show d63a490 -- render.yaml | head -30","description":"Inspect the pin commit"}`))),
		bubbles: []storeRow{
			tool{id: "shell", name: "run_terminal_command_v2", result: "render.yaml | 2 +-",
				params: `{"command":"git show d63a490 -- render.yaml | head -30","cwd":"","parsingResult":{"commands":[{"words":["git","show","d63a490","render.yaml","head"]}]},"requestedSandboxPolicy":{"workspace":"/work/api"},"commandDescription":"Inspect the pin commit"}`}.row(),
			toolRow("read", "read_file_v2", `{"path":"render.yaml"}`, "services:\n  - type: web"),
		},
		want: map[int][]string{1: {"read"}, 2: {"shell"}},
		check: func(t *testing.T, d transforms.Derived, _ []map[string]any) {
			assert.Zero(t, d.Repeats, "the shell call lost its own bubble")
		},
	}, {
		// One shared value among several is corroboration, not identity.
		name:       "a lone shared value among several is not identity",
		query:      "check the backends",
		transcript: jsonl(turn(use("Grep", `{"pattern":"StatusConflict|409","path":"/work/api"}`)), turn(use("Grep", `{"pattern":"PurgePrefix|VerifyCapabilities","path":"/work/api"}`))),
		bubbles: []storeRow{
			toolRow("purge", "ripgrep_raw_search", `{"pattern":"PurgePrefix|VerifyCapabilities","path":"/work/api"}`, "purge.go:14"),
			toolRow("conflict", "ripgrep_raw_search", `{"pattern":"StatusConflict|409","path":"/work/api"}`, "s3.go:88"),
		},
		want: map[int][]string{1: {"conflict"}, 2: {"purge"}},
	}})
}

const (
	etagArgs  = `{"pattern":"quoteETag|normaliseETag","path":"/work/api/internal/backends/s3"}`
	purgeArgs = `{"pattern":"PurgePrefix|opts\\.Prefix","path":"/work/api/internal/backends/s3"}`
)

// A call the store recorded once attaches and consumes nothing: it hides no hole, takes no bubble.
func TestRepeatedCalls(t *testing.T) {
	var farBack []string
	var farBackRows []storeRow
	for i := range 64 {
		args := fmt.Sprintf(`{"pattern":"sym%02dAlpha|sym%02dBeta","path":"/work/api/pkg%02d"}`, i, i, i)
		farBack = append(farBack, turn(use("Grep", args)))
		farBackRows = append(farBackRows, toolRow(fmt.Sprintf("fill%02d", i), "ripgrep_raw_search", args, "pkg.go:1"))
	}
	var subagent []storeRow
	for i := range 20 {
		subagent = append(subagent, toolRow(fmt.Sprintf("sub%02d", i), "read_file_v2", fmt.Sprintf(`{"path":"/work/api/sub/f%02d.go"}`, i), "subagent detail"))
	}
	const resolve = `{"path":"/work/api/internal/config/resolve.go"}`

	runJoinCases(t, []joinCase{{
		// The re-run could be the argument-less bubble, so it ships undecided and that bubble stays barred.
		name:       "a call the store recorded once is not a mismatch",
		query:      "look twice",
		transcript: jsonl(turn(use("Grep", etagArgs)), turn(use("Grep", etagArgs)), turn(use("Grep", purgeArgs)), turn(text("Both live in etag.go."))),
		bubbles: []storeRow{
			toolRow("grep", "ripgrep_raw_search", etagArgs, "etag.go:9"),
			toolRow("argless", "ripgrep_raw_search", `{}`, "NOT THE REPEAT'S RESULT"),
			toolRow("purge", "ripgrep_raw_search", purgeArgs, "purge.go:14"),
			said("a1", "Both live in etag.go."),
		},
		want:   map[int][]string{1: {"grep"}, 2: {}, 3: {"purge"}, 4: {"a1"}},
		absent: []string{"NOT THE REPEAT'S RESULT"},
	}, {
		// The repeat consumes nothing, so the next consuming match is what catches the hole.
		name:  "a hole before a repeat is a mismatch, not tail",
		query: "look around",
		transcript: jsonl(turn(use("Grep", etagArgs)), turn(use("Grep", `{"pattern":"nothing|the|store|knows","path":"/work/api/internal/engine"}`)),
			turn(use("Grep", etagArgs)), turn(use("Grep", purgeArgs))),
		bubbles:  []storeRow{toolRow("grep", "ripgrep_raw_search", etagArgs, "etag.go:9"), toolRow("purge", "ripgrep_raw_search", purgeArgs, "purge.go:14")},
		mismatch: true,
	}, {
		// Only an identity-grade consumer makes a later agreeing call a repeat, not a positional one.
		name:       "a fallback consumer does not make the next call a repeat",
		query:      "scan the backends",
		transcript: jsonl(turn(use("Grep", `{"pattern":"PurgePrefix|opts","path":"/work/s3"}`)), turn(use("Grep", `{"pattern":"quoteETag|normaliseETag","path":"/work/s3"}`)), turn(text("Both live in etag.go."))),
		bubbles:    []storeRow{toolRow("etag", "ripgrep_raw_search", `{"pattern":"quoteETag|normaliseETag","path":"/work/s3"}`, "etag.go:9"), said("a1", "Both live in etag.go.")},
		mismatch:   true,
	}, {
		// Left unconsumed, the undecided bubble became the next terminal command's positional fallback.
		name:       "an undecidable repeat attaches nothing and does not cascade",
		query:      "run the tests",
		transcript: jsonl(turn(use("Shell", `{"command":"npm test"}`)), turn(use("Shell", `{"command":"npm test"}`)), turn(use("Shell", `{"command":"git status --short"}`))),
		bubbles: []storeRow{
			toolRow("t1", "run_terminal_command_v2", `{"command":"npm test"}`, "FAIL: 3 failing"),
			toolRow("t2", "run_terminal_command_v2", `{}`, "PASS all tests passed"),
			toolRow("t3", "run_terminal_command_v2", `{}`, " M internal/config/resolve.go"),
		},
		want:   map[int][]string{2: {}, 3: {"t3"}},
		absent: []string{"PASS all tests passed"},
		check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
			assert.Equal(t, " M internal/config/resolve.go", matchedBubbles(t, lines[3], "t3")[0]["result"])
		},
	}, {
		// Repeat detection is not bounded by the look-behind window: 64 consumed calls sit in between.
		name:       "a store-deduped re-run far back is still a repeat",
		query:      "audit everything",
		transcript: jsonl(append(append([]string{turn(use("Grep", etagArgs))}, farBack...), turn(use("Grep", etagArgs)), turn(text("All checks passed.")))...),
		bubbles:    append(append([]storeRow{toolRow("grep", "ripgrep_raw_search", etagArgs, "etag.go:9")}, farBackRows...), said("a1", "All checks passed.")),
		infos:      []string{"repeat"},
	}, {
		// The re-run's own bubble sits past the forward window, so the store did record it.
		name:       "a repeat whose own bubble is out of reach is a mismatch",
		query:      "check the resolver twice",
		transcript: jsonl(turn(use("Read", resolve)), turn(use("Read", resolve)), turn(text("The resolver is bounded."))),
		bubbles: append(append([]storeRow{toolRow("r1", "read_file_v2", resolve, "package config"), said("prose", "The resolver is bounded.")},
			subagent...), toolRow("r2", "read_file_v2", resolve, "package config, again")),
		mismatch: true,
	}, {
		// Injected turns get no bubble; a trailing repeat must not turn that tail into a mismatch.
		name:  "a trailing repeat does not turn injected turns into mismatches",
		query: "find the etag helpers",
		transcript: jsonl(turn(use("Grep", etagArgs)), `{"type":"turn_ended","status":"success"}`,
			`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 3, 2026, 9:05 AM (UTC+2)</timestamp>\n\n<user_query>Briefly inform the user about the task result.</user_query>"}]}}`,
			turn(use("Grep", etagArgs)), `{"type":"turn_ended","status":"success"}`),
		bubbles: []storeRow{toolRow("grep", "ripgrep_raw_search", etagArgs, "etag.go:9")},
		infos:   []string{"extend past the store", "repeat"},
	}})
}

// How a tool block's arguments pick its bubble when store and transcript order disagree.
func TestArgumentEvidence(t *testing.T) {
	runJoinCases(t, []joinCase{{
		// Two agreeing values do not outvote a third: under two-of-three these searches swapped results.
		name:       "calls sharing all but one argument do not swap",
		query:      "check the goldens",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"ParseManifest|writeManifest","path":"/work/api/handlers","glob":"*.golden"}},{"type":"tool_use","name":"Grep","input":{"pattern":"VerifyPayload|checkPayload","path":"/work/api/handlers","glob":"*.golden"}}]}}`,
		bubbles: []storeRow{
			toolRow("gb", "ripgrep_raw_search", `{"pattern":"VerifyPayload|checkPayload","path":"/work/api/handlers","glob":"*.golden"}`, "payload.go:70"),
			toolRow("ga", "ripgrep_raw_search", `{"pattern":"ParseManifest|writeManifest","path":"/work/api/handlers","glob":"*.golden"}`, "manifest.go:12"),
		},
		want: map[int][]string{1: {"ga", "gb"}},
	}, {
		// A value too long to be a coincidence outranks a bubble with nothing recorded.
		name:       "a partially agreeing own bubble outranks an evidence-free orphan",
		query:      "check types",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Task","input":{"prompt":"Run npx tsc --noEmit in the repo and report every error.","subagent_type":"general-purpose"}}]}}`,
		bubbles: []storeRow{
			tool{id: "orphan", name: "task_v2", status: "error", rawArgs: "{}", result: "NOT THIS TASK'S RESULT"}.row(),
			tool{id: "task", name: "task_v2", result: "no type errors",
				params: `{"prompt":"Run npx tsc --noEmit in the repo and report every error.","subagentType":"SUBAGENT_EXECUTION_ENVIRONMENT_UNSPECIFIED"}`}.row(),
		},
		want: map[int][]string{1: {"task"}},
		check: func(t *testing.T, _ transforms.Derived, lines []map[string]any) {
			e := matchedBubbles(t, lines[1], "task")[0]
			assert.Equal(t, "completed", e["status"])
			assert.Equal(t, "no type errors", e["result"])
		},
	}, {
		// A value shorter than minArgLen cannot confirm identity but can deny it.
		name:  "a short distinguishing argument separates two calls",
		query: "sweep the api",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"err","path":"/work/api"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"StatusConflict|409","path":"/work/api"}}]}}`,
		bubbles: []storeRow{
			toolRow("conflict", "ripgrep_raw_search", `{"pattern":"StatusConflict|409","path":"/work/api"}`, "s3.go:88"),
			toolRow("errown", "ripgrep_raw_search", `{"pattern":"err","path":"/work/api"}`, "everywhere.go:1"),
		},
		want: map[int][]string{1: {"errown"}, 2: {"conflict"}},
	}, {
		// Numbers compare as parsed values, and a disagreeing offset vetoes though it confirms nothing.
		name:       "a numeric argument vetoes a borrowed identity",
		query:      "read both halves",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/engine/engine.go","offset":400}},{"type":"tool_use","name":"Read","input":{"path":"/work/api/internal/engine/engine.go","offset":0}}]}}`,
		bubbles: []storeRow{
			toolRow("r0", "read_file_v2", `{"path":"/work/api/internal/engine/engine.go","offset":0}`, "the head of the file"),
			toolRow("r400", "read_file_v2", `{"path":"/work/api/internal/engine/engine.go","offset":400}`, "the tail of the file"),
		},
		want: map[int][]string{1: {"r400", "r0"}},
	}, {
		// Comparing only top-level strings left nested inputs neutral, so two calls swapped on position.
		name:       "nested input values are argument evidence",
		query:      "apply both edits",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"MultiEdit","input":{"edits":[{"file_path":"/work/api/internal/config/resolve.go","old_string":"the bound is unchecked here","new_string":"the bound is enforced here"}]}},{"type":"tool_use","name":"MultiEdit","input":{"edits":[{"file_path":"/work/api/internal/config/load.go","old_string":"loads the whole file eagerly","new_string":"streams the file in pages"}]}}]}}`,
		bubbles: []storeRow{
			toolRow("e2", "multi_edit_v2", `{"edits":[{"file_path":"/work/api/internal/config/load.go","old_string":"loads the whole file eagerly","new_string":"streams the file in pages"}]}`, "1 edit applied to load.go"),
			toolRow("e1", "multi_edit_v2", `{"edits":[{"file_path":"/work/api/internal/config/resolve.go","old_string":"the bound is unchecked here","new_string":"the bound is enforced here"}]}`, "1 edit applied to resolve.go"),
		},
		want: map[int][]string{1: {"e1", "e2"}},
	}, {
		// Of two fully agreeing bubbles, the one agreeing on more is the call.
		name:       "the stronger of two agreeing bubbles wins",
		query:      "find the decompression bound",
		transcript: `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"maxDecompressedBytes|decompressLimitCeiling","path":"/work/api/internal/backends/s3/multipart"}}]}}`,
		bubbles: []storeRow{
			toolRow("ls", "run_terminal_command_v2", `ls -la /work/api/internal/backends/s3/multipart`, "NOT THE SEARCH'S RESULT"),
			toolRow("grep", "ripgrep_raw_search", `{"pattern":"maxDecompressedBytes|decompressLimitCeiling","path":"/work/api/internal/backends/s3/multipart"}`, "multipart.go:88"),
		},
		want: map[int][]string{1: {"grep"}},
	}})
}
