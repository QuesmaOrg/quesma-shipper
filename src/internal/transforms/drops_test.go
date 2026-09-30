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

// dropShapes are the value forms seen at drop paths: std base64, a data URL and a Fernet token.
func dropShapes() map[string]string {
	return map[string]string{
		"base64":   opaqueBlob(1, 4096, base64.StdEncoding),
		"data-url": "data:image/png;base64," + opaqueBlob(2, 4096, base64.StdEncoding),
		"fernet":   fernetToken(3, 2048),
	}
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

func forEachDropCase(t *testing.T, f func(t *testing.T, family, path, value string)) {
	shapes := dropShapes()
	for family, paths := range transforms.CompiledDrops() {
		for _, path := range paths {
			for shape, value := range shapes {
				t.Run(family+"/"+path+"/"+shape, func(t *testing.T) { f(t, family, path, value) })
			}
		}
	}
}

func TestEveryDropPathBecomesItsSentinel(t *testing.T) {
	s := newScrubber(t)
	forEachDropCase(t, func(t *testing.T, family, path, value string) {
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

// Every entry's premise: without the drop the entropy backstop already destroys the value.
func TestEveryDropPathIsDestroyedWithoutTheDrop(t *testing.T) {
	s := scrubberWith(t, func(cfg *transforms.Config) { cfg.Drops = nil })
	forEachDropCase(t, func(t *testing.T, family, path, value string) {
		res := scrubJSONL(t, s, family, buildRecordWithValueAt(t, path, value)+"\n")
		if strings.Contains(string(res.Out.Bytes()), value) {
			t.Fatalf("the value ships intact without the drop; dropping it would lose data")
		}
		if res.RuleHits["generic-entropy"] == 0 {
			t.Errorf("expected the entropy backstop to shred the value: %v", res.RuleHits)
		}
	})
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

// A value at a drop path that is not an opaque blob takes the full ladder, as before.
func TestDropPathValueThatIsNotABlobIsScrubbedAsBefore(t *testing.T) {
	s := newScrubber(t)
	b64 := opaqueBlob(30, 600, base64.StdEncoding)
	cases := []struct {
		name, family, line string
		intact             bool
	}{
		{"text document", "claude-code",
			`{"message":{"content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Meeting notes: ship the drop list on Tuesday."}}]}}`, true},
		{"https image url", "codex",
			`{"payload":{"type":"message","content":[{"type":"input_image","image_url":"https://example.com/cat.png"}]}}`, true},
		{"number", "claude-code", `{"message":{"content":[{"type":"thinking","signature":12345}]}}`, true},
		{"object", "codex", `{"payload":{"result":{"Ok":{"content":[{"type":"text","text":"done"}]}}}}`, true},
		{"short base64", "claude-code",
			`{"message":{"content":[{"type":"redacted_thinking","data":"` + b64[:120] + `"}]}}`, false},
		{"mixed alphabets", "claude-code",
			`{"message":{"content":[{"type":"thinking","signature":"` + b64[:300] + "-_" + b64[300:] + `"}]}}`, false},
		{"padding inside", "claude-code",
			`{"toolUseResult":{"file":{"base64":"` + b64[:300] + "==" + b64[300:] + `"}}}`, false},
		{"escaped", "claude-code",
			`{"toolUseResult":{"file":{"base64":"` + b64[:300] + `\u0041` + b64[300:] + `"}}}`, false},
		{"data url without base64", "codex",
			`{"payload":{"type":"message","content":[{"type":"input_image","image_url":"data:text/plain,` + b64 + `"}]}}`, false},
		{"data url with prose type", "codex",
			`{"payload":{"type":"message","content":[{"type":"input_image","image_url":"data:see this;base64,` + b64 + `"}]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scrubJSONL(t, s, tc.family, tc.line+"\n")
			if res.RuleHits[transforms.Dropped] != 0 {
				t.Fatalf("dropped: %v\n%s", res.RuleHits, res.Out.Bytes())
			}
			if intact := string(res.Out.Bytes()) == tc.line+"\n"; intact != tc.intact {
				t.Errorf("intact = %v, want %v:\n%s", intact, tc.intact, res.Out.Bytes())
			}
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

func TestDropIsIdempotent(t *testing.T) {
	s := newScrubber(t)
	line := buildRecordWithValueAt(t, "message.content[].content[].source.data", opaqueBlob(60, 5000, base64.StdEncoding))
	first := scrubJSONL(t, s, "claude-code", line+"\n")
	second := scrubJSONL(t, s, "claude-code", string(first.Out.Bytes()))
	if string(second.Out.Bytes()) != string(first.Out.Bytes()) || len(second.RuleHits) != 0 {
		t.Errorf("second pass changed the output or hit rules: %v\n%s", second.RuleHits, second.Out.Bytes())
	}
}

func TestDuplicateKeysAreBothDropped(t *testing.T) {
	s := newScrubber(t)
	a, b := opaqueBlob(70, 900, base64.StdEncoding), opaqueBlob(71, 900, base64.StdEncoding)
	res := scrubJSONL(t, s, "claude-code", `{"toolUseResult":{"file":{"base64":"`+a+`","base64":"`+b+`"}}}`+"\n")
	if res.RuleHits[transforms.Dropped] != 2 {
		t.Errorf("rule hits = %v", res.RuleHits)
	}
	assertNoFragment(t, res.Out.Bytes(), a)
	assertNoFragment(t, res.Out.Bytes(), b)
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
