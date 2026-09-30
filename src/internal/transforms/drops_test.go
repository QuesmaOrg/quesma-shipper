package transforms_test

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/rand"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// opaqueBlob is random rather than repeated so the entropy backstop would shred it.
func opaqueBlob(seed int64, n int, enc *base64.Encoding) string {
	return randBase64(rand.New(rand.NewSource(seed)), n, enc)
}

func fernetToken(seed int64, n int) string {
	return randFernet(rand.New(rand.NewSource(seed)), n)
}

func scrubberWith(t testing.TB, edit func(*transforms.Config)) *transforms.Scrubber {
	t.Helper()
	cfg := transforms.DefaultConfig()
	cfg.Username = "jane"
	edit(&cfg)
	s, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// valueAt reads the value at a field path, taking the first element of each array.
func valueAt(t *testing.T, line []byte, path string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		t.Fatalf("scrubbed output is not valid JSON: %v\n%s", err, line)
	}
	for _, seg := range strings.Split(path, ".") {
		key, isArray := strings.CutSuffix(seg, "[]")
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no object at %q in %s", seg, line)
		}
		v = obj[key]
		if isArray {
			arr, ok := v.([]any)
			if !ok || len(arr) == 0 {
				t.Fatalf("no array at %q in %s", seg, line)
			}
			v = arr[0]
		}
	}
	return v
}

// assertNoFragment fails when any 16-byte window of the original ships.
func assertNoFragment(t *testing.T, out []byte, original string) {
	t.Helper()
	for i := 0; i+16 <= len(original); i++ {
		if strings.Contains(string(out), original[i:i+16]) {
			t.Fatalf("a fragment of the dropped value shipped: %q", original[i:i+16])
		}
	}
}

var droppedSentinel = transforms.Sentinel(transforms.Dropped)

func TestEveryDropPathBecomesItsSentinel(t *testing.T) {
	s := newScrubber(t)
	value := opaqueBlob(1, 4096, base64.StdEncoding)
	for family, paths := range transforms.CompiledDrops() {
		for _, path := range paths {
			t.Run(family+"/"+path, func(t *testing.T) {
				res := scrubJSONL(t, s, family, buildRecordWithValueAt(t, path, value)+"\n")
				out := res.Out.Bytes()
				if got := valueAt(t, out, path); got != droppedSentinel {
					t.Fatalf("value at %s = %.60q, want %s", path, got, droppedSentinel)
				}
				if want := map[string]int{transforms.Dropped: 1}; !maps.Equal(res.RuleHits, want) {
					t.Errorf("rule hits = %v, want %v", res.RuleHits, want)
				}
				if res.BytesRedacted != len(value) {
					t.Errorf("bytes redacted = %d, want the whole value, %d", res.BytesRedacted, len(value))
				}
				assertNoFragment(t, out, value)
			})
		}
	}
}

func TestClaudeCodeDropKeepsTheRestOfTheBlock(t *testing.T) {
	s := newScrubber(t)
	const toolUseID = "toolu_0183yENGzL6di289E8QTzyxi"
	signature := opaqueBlob(10, 1500, base64.StdEncoding)
	image := opaqueBlob(11, 8192, base64.StdEncoding)

	assistant := `{"type":"assistant","uuid":"a1","message":{"content":[` +
		`{"type":"thinking","thinking":"Plan: rotate ghp_abcdefghijklmnopqrstuvwxyz0123456789 first.","signature":"` + signature + `"},` +
		`{"type":"tool_use","id":"` + toolUseID + `","name":"Read","input":{"file_path":"/tmp/shot.png"}}]}}`
	user := `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"AKIAIOSFODNN7EXAMPLE","data":"` + image + `"}}]}]},` +
		`"toolUseResult":{"type":"image","file":{"base64":"` + image + `","type":"image/png","originalSize":6144}}}`

	res := scrubJSONL(t, s, "claude-code", assistant+"\n"+user+"\n")
	out := string(res.Out.Bytes())
	lines := strings.Split(strings.TrimSpace(out), "\n")

	for _, want := range []string{
		`"type":"thinking","thinking":"Plan: rotate __REDACTED:github-pat__ first."`,
		`"signature":"` + droppedSentinel + `"`,
		`"id":"` + toolUseID + `"`, `"tool_use_id":"` + toolUseID + `"`,
		`"source":{"type":"base64","media_type":"__REDACTED:aws-access-key-id__","data":"` + droppedSentinel + `"}`,
		`"file":{"base64":"` + droppedSentinel + `","type":"image/png","originalSize":6144}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	want := map[string]int{transforms.Dropped: 3, "github-pat": 1, "aws-access-key-id": 1}
	if !maps.Equal(res.RuleHits, want) {
		t.Errorf("rule hits = %v, want %v", res.RuleHits, want)
	}
	if len(lines) != 2 || valueAt(t, []byte(lines[1]), "toolUseResult.file.type") != "image/png" {
		t.Errorf("records did not survive as valid JSON:\n%s", out)
	}
	assertNoFragment(t, res.Out.Bytes(), signature)
	assertNoFragment(t, res.Out.Bytes(), image)
}

func TestCodexDropKeepsTheRestOfTheItem(t *testing.T) {
	s := newScrubber(t)
	fernet := fernetToken(20, 1200)
	image := "data:image/jpeg;base64," + opaqueBlob(21, 6000, base64.StdEncoding)

	payload := `{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"key sk-proj-abcdefghijklmnopqrstuvwxyz0123456789 leaked"}],"encrypted_content":"` + fernet + `"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_abc","output":[` +
		`{"type":"input_text","text":"see screenshot"},{"type":"input_image","image_url":"` + image + `"}]}}` + "\n"

	res := scrubJSONL(t, s, "codex", payload)
	out := string(res.Out.Bytes())
	for _, want := range []string{
		`"text":"key __REDACTED:openai-api-key__ leaked"`,
		`"encrypted_content":"` + droppedSentinel + `"`,
		`"call_id":"call_abc"`, `{"type":"input_text","text":"see screenshot"}`,
		`{"type":"input_image","image_url":"` + droppedSentinel + `"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	want := map[string]int{transforms.Dropped: 2, "openai-api-key": 1}
	if !maps.Equal(res.RuleHits, want) {
		t.Errorf("rule hits = %v, want %v", res.RuleHits, want)
	}
}

const plantedPAT = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// A short string or a non-string at a drop path takes the full ladder, as before.
func TestShortValueAtADropPathIsScrubbedAsBefore(t *testing.T) {
	s := newScrubber(t)
	cases := []struct {
		name, family, line string
		// The rule that redacts part of the value; empty when the record ships intact.
		rule string
	}{
		{"https image url with a secret", "codex",
			`{"payload":{"type":"message","content":[{"type":"input_image","image_url":"https://example.com/cat.png?token=` + plantedPAT + `"}]}}`, "github-pat"},
		{"number", "claude-code", `{"message":{"content":[{"type":"thinking","signature":12345}]}}`, ""},
		{"object", "codex", `{"payload":{"result":{"Ok":{"content":[{"type":"text","text":"done"}]}}}}`, ""},
		{"one byte under the floor", "claude-code",
			`{"toolUseResult":{"file":{"base64":"` + strings.Repeat("note ", 32)[:159] + `"}}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scrubJSONL(t, s, tc.family, tc.line+"\n")
			if res.RuleHits[transforms.Dropped] != 0 {
				t.Fatalf("dropped: %v\n%s", res.RuleHits, res.Out.Bytes())
			}
			if intact := string(res.Out.Bytes()) == tc.line+"\n"; intact != (tc.rule == "") {
				t.Errorf("intact = %v, want %v:\n%s", intact, tc.rule == "", res.Out.Bytes())
			}
			if tc.rule != "" && res.RuleHits[tc.rule] == 0 {
				t.Errorf("expected %s to scrub the value: %v", tc.rule, res.RuleHits)
			}
			if strings.Contains(string(res.Out.Bytes()), plantedPAT) {
				t.Error("the secret shipped")
			}
		})
	}
}

// A string from the floor up is dropped whole whatever it holds, so a secret in it never meets a rule.
func TestLongStringAtADropPathIsDroppedWhateverItHolds(t *testing.T) {
	s := newScrubber(t)
	b64 := opaqueBlob(30, 600, base64.StdEncoding)
	cases := []struct{ name, family, path, body string }{
		{"plaintext with a secret", "claude-code", "message.content[].source.data",
			strings.Repeat("Meeting notes: ship the drop list on Tuesday. ", 4) + "Token " + plantedPAT + "."},
		{"at the floor", "claude-code", "toolUseResult.file.base64", strings.Repeat("note ", 32)},
		// The ledger counts decoded bytes: these 18 raw bytes of escapes decode to 6.
		{"escaped", "claude-code", "toolUseResult.file.base64", b64[:300] + `\u0041\n\"\\\u00e9` + b64[300:]},
		// The floor is on the raw body: 310 bytes here, which decode to 155.
		{"escaped under the floor once decoded", "claude-code", "toolUseResult.file.base64", strings.Repeat(`note\u0020`, 31)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := strings.Replace(buildRecordWithValueAt(t, tc.path, "BODY"), "BODY", tc.body, 1)
			value, ok := valueAt(t, []byte(line), tc.path).(string)
			if !ok {
				t.Fatalf("no string at %s in %s", tc.path, line)
			}
			res := scrubJSONL(t, s, tc.family, line+"\n")
			out := res.Out.Bytes()
			if got := valueAt(t, out, tc.path); got != droppedSentinel {
				t.Fatalf("value at %s = %.60q, want %s", tc.path, got, droppedSentinel)
			}
			if want := map[string]int{transforms.Dropped: 1}; !maps.Equal(res.RuleHits, want) {
				t.Errorf("rule hits = %v, want %v", res.RuleHits, want)
			}
			if res.BytesRedacted != len(value) {
				t.Errorf("bytes redacted = %d, want the decoded value, %d", res.BytesRedacted, len(value))
			}
			assertNoFragment(t, out, value)
		})
	}
}

func TestDropsAreScopedToTheirFamilyAndTheOuterRecord(t *testing.T) {
	s := newScrubber(t)
	blob := opaqueBlob(40, 3000, base64.StdEncoding)
	embedded, err := json.Marshal(`{"encrypted_content":"` + blob + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, family, line string }{
		{"claude path under codex", "codex", buildRecordWithValueAt(t, "toolUseResult.file.base64", blob)},
		{"codex path under claude-code", "claude-code", buildRecordWithValueAt(t, "payload.encrypted_content", blob)},
		{"cursor has no drops", "cursor", buildRecordWithValueAt(t, "message.content[].signature", blob)},
		{"no family", "", buildRecordWithValueAt(t, "message.content[].signature", blob)},
		// The path inside is payload#json.encrypted_content, never payload.encrypted_content.
		{"embedded document", "codex", `{"payload":` + string(embedded) + `}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scrubJSONL(t, s, tc.family, tc.line+"\n")
			if res.RuleHits[transforms.Dropped] != 0 {
				t.Errorf("dropped outside its scope: %v\n%s", res.RuleHits, res.Out.Bytes())
			}
		})
	}
}

// Drops and exemptions pull opposite ways, so no compiled path may be both.
func TestDropsAreDisjointFromExemptions(t *testing.T) {
	exempt := transforms.NewExemptionSet(transforms.CompiledExemptions())
	for family, paths := range transforms.CompiledDrops() {
		for _, path := range paths {
			if exempt.Exempt(family, transforms.FieldPath(path)) {
				t.Errorf("%s %s is both dropped and exempt", family, path)
			}
		}
	}
	// Cursor's hex image stays exempt and intact (TestDeclaredOpaqueBinaryPayloadSurvivesTheEntropyBackstop).
	if len(transforms.CompiledDrops()["cursor"]) != 0 {
		t.Error("cursor's opaque payload is exempt and survives intact; it must not be dropped")
	}
}

// A served or owner structural_exempt entry keeps its meaning: the value ships intact.
func TestAConfiguredExemptionOutranksADrop(t *testing.T) {
	const path = "message.content[].source.data"
	image := opaqueBlob(80, 30000, base64.StdEncoding)
	line := buildRecordWithValueAt(t, path, image) + "\n"
	for _, family := range []string{"claude-code", "*"} {
		t.Run(family, func(t *testing.T) {
			s := scrubberWith(t, func(cfg *transforms.Config) {
				cfg.Exemptions[family] = append(cfg.Exemptions[family], path)
			})
			res := scrubJSONL(t, s, "claude-code", line)
			if string(res.Out.Bytes()) != line || len(res.RuleHits) != 0 {
				t.Errorf("exempt value did not ship intact: %v", res.RuleHits)
			}
		})
	}
}

// Torn tails and raw-text payloads have no field paths, so they are scanned exactly as before.
func TestTornTailAndRawTextAreNotDropped(t *testing.T) {
	s := newScrubber(t)
	sig := opaqueBlob(50, 1500, base64.StdEncoding)
	whole := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"` + sig + `"}]}}`
	torn := whole[:len(whole)/2]

	res := scrubJSONL(t, s, "claude-code", whole+"\n"+torn)
	if res.ScanMode != transforms.ScanModeMixed || res.LinesRawScanned != 1 {
		t.Fatalf("scan mode = %s, raw lines = %d", res.ScanMode, res.LinesRawScanned)
	}
	if res.RuleHits[transforms.Dropped] != 1 {
		t.Errorf("only the parsed line drops: %v", res.RuleHits)
	}
	if !strings.HasSuffix(string(res.Out.Bytes()), torn) {
		t.Error("the torn tail must ship as the raw scanner leaves it")
	}

	raw, err := s.Scrub([]byte(whole+"\n"), transforms.Hint{Family: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if raw.ScanMode != transforms.ScanModeRawText || len(raw.RuleHits) != 0 {
		t.Errorf("raw text mode changed: %s %v", raw.ScanMode, raw.RuleHits)
	}
}

func TestNewRejectsADropThatCannotApply(t *testing.T) {
	for name, drops := range map[string]map[string][]string{
		"every family": {"*": {"data"}},
		"empty path":   {"codex": {""}},
		"embedded":     {"codex": {"payload#json.encrypted_content"}},
		"duplicate":    {"codex": {"payload.result", "payload.result"}},
	} {
		cfg := transforms.DefaultConfig()
		cfg.Drops = drops
		if _, err := transforms.New(cfg); err == nil {
			t.Errorf("%s: New accepted %v", name, drops)
		}
	}
}
