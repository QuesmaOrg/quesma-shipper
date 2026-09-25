package transforms

import (
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

func sourcePatchScrubber(t *testing.T) *Scrubber {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Exemptions = CompiledExemptions()
	cfg.Username = "jane"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJSONTextAcceptanceMatchesEncodingJSON(t *testing.T) {
	s := sourcePatchScrubber(t)
	for _, line := range []string{
		`null`, `true`, `-0`, `1e+9`, `"text"`, `[]`, `{}`,
		` { "a" : [1, {"b":"x"}], "a": 2 } `,
		`"\"\\\/\b\f\n\r\t\u0041\ud83d\ude00\ud800"`,
		"\"a\xffb\"",
		``, ` `, `{`, `[1,]`, `{"a":}`, `01`, `+1`, `.1`, `NaN`,
		`true false`, `"\q"`, "\"\n\"", `{"a":1,}`,
	} {
		var scan packs.ValueScan
		var walker jsonWalker
		walker.reset(s, "claude-code", &scan, []byte(line))
		got := walker.walkLine() == nil
		if want := json.Valid([]byte(line)); got != want {
			t.Errorf("valid(%q) = %v, encoding/json = %v", line, got, want)
		}
	}
}

func TestSourcePatchingRequotesOnlyDirtyStrings(t *testing.T) {
	s := sourcePatchScrubber(t)
	before := " \t{ \"n\" : 1e+09, \"text\" : \"left\\/\\u003c AKI\\u0041IOSFODNN7EXAMPLE right\", \"keep\":\"\\u0041\\/\" } \r\n"
	want := " \t{ \"n\" : 1e+09, \"text\" : \"left/< __REDACTED:aws-access-key-id__ right\", \"keep\":\"\\u0041\\/\" } \r\n"

	res, err := s.Scrub([]byte(before), Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Out.Bytes()); got != want {
		t.Fatalf("source bytes changed outside the dirty string:\n got %q\nwant %q", got, want)
	}
	if res.ScanMode != ScanModeDecodedJSON || res.RuleHits["aws-access-key-id"] != 1 {
		t.Fatalf("unexpected result metadata: %+v", res)
	}
}

func TestSourcePatchingCoversKeysDuplicatesAndBareStrings(t *testing.T) {
	s := sourcePatchScrubber(t)
	for _, tc := range []struct {
		name, before, want string
	}{
		{
			name:   "escaped key",
			before: `{"AKI\u0041IOSFODNN7EXAMPLE":"kept"}`,
			want:   `{"__REDACTED:aws-access-key-id__":"kept"}`,
		},
		{
			name:   "duplicate values",
			before: `{"a":"AKIAIOSFODNN7EXAMPLE","a":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`,
			want:   `{"a":"__REDACTED:aws-access-key-id__","a":"__REDACTED:github-pat__"}`,
		},
		{
			name:   "leading whitespace before bare string",
			before: `  "AKI\u0041IOSFODNN7EXAMPLE" `,
			want:   `  "__REDACTED:aws-access-key-id__" `,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.Scrub([]byte(tc.before), Hint{Family: "claude-code", JSONL: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(res.Out.Bytes()); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			if res.ScanMode != ScanModeDecodedJSON {
				t.Fatalf("scan mode = %q", res.ScanMode)
			}
		})
	}
}

func TestSourcePatchingNormalizesExceptionalBytesInDirtyStrings(t *testing.T) {
	s := sourcePatchScrubber(t)
	for _, tc := range []struct {
		name, prefix string
	}{
		{"invalid UTF-8", "\xff"},
		{"unpaired surrogate", `\ud800`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := `{"text":"` + tc.prefix + ` / AKIAIOSFODNN7EXAMPLE / \u0041"}`
			want := `{"text":"� / __REDACTED:aws-access-key-id__ / A"}`
			res, err := s.Scrub([]byte(before), Hint{Family: "claude-code", JSONL: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(res.Out.Bytes()); got != want {
				t.Fatalf("got  %q\nwant %q", got, want)
			}
		})
	}
}

// A string value is walked as JSON exactly when encoding/json would accept it, so the nested
// walk inherits the line walk's acceptance parity.
func TestEmbeddedJSONAcceptanceMatchesEncodingJSON(t *testing.T) {
	s := sourcePatchScrubber(t)
	for _, doc := range []string{
		`[]`, `{}`, ` { "a" : [1, {"b":"x"}], "a": 2 } `, `[1e+9, -0, null]`,
		"[\"a\xffb\"]", `[`, `[1,]`, `{"a":}`, `[01]`, `[NaN]`, `[true false]`, `["\q"]`, "[\"\n\"]", `{"a":1,}`,
	} {
		inner := `[` + doc + `,{"password":"hunter2"}]`
		line, err := json.Marshal(map[string]string{"out": inner})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Scrub(append(line, '\n'), Hint{Family: "codex", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		walked := res.RuleHits["key-name"] == 1
		if want := json.Valid([]byte(inner)); walked != want {
			t.Errorf("walked(%q) = %v, encoding/json valid = %v", inner, walked, want)
		}
	}
}

// Exemptions are exact paths: a field encoded inside an exempt string is not itself exempt.
func TestEmbeddedFieldsDoNotInheritOuterExemptions(t *testing.T) {
	s := sourcePatchScrubber(t)
	const token = "tOk3nZq8vLmXw2Rf5uJhPd0aYc6eBn1KsG9iD4"
	for value, survives := range map[string]bool{
		token:                    true,
		`{"id":"` + token + `"}`: false,
	} {
		line, err := json.Marshal(map[string]any{"payload": map[string]string{"call_id": value}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Scrub(append(line, '\n'), Hint{Family: "codex", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(res.Out.Bytes()), token); got != survives {
			t.Errorf("call_id %q: token survived = %v, want %v", value, got, survives)
		}
	}
}

// The nesting bound is shared with the outer line: a document past it is scanned as text, not
// rejected, and the line still counts as parsed.
func TestEmbeddedJSONPastTheDepthBoundIsScannedAsText(t *testing.T) {
	s := sourcePatchScrubber(t)
	value, err := json.Marshal(strings.Repeat("[", 20) + `{"password":"hunter2"}` + strings.Repeat("]", 20))
	if err != nil {
		t.Fatal(err)
	}
	for _, outer := range []int{1, maxDepth - 5} {
		line := strings.Repeat("[", outer) + string(value) + strings.Repeat("]", outer) + "\n"
		res, err := s.Scrub([]byte(line), Hint{Family: "codex", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.LinesParsed != 1 {
			t.Fatalf("outer depth %d: line not parsed: %+v", outer, res)
		}
		if walked := res.RuleHits["key-name"] == 1; walked != (outer == 1) {
			t.Errorf("outer depth %d: inner document walked = %v", outer, walked)
		}
	}
}

// An escape-dense value (a minified file encoded twice) costs a small multiple of its own size:
// the shadow copy and the boundary bits.
func TestEscapeShadowMemoryIsLinearInTheValue(t *testing.T) {
	for _, value := range []string{strings.Repeat(`\n`, 1<<19), strings.Repeat(`{\"a\":1},`, 1<<17)} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		escapeShadow(value)
		runtime.ReadMemStats(&after)
		if got, limit := after.TotalAlloc-before.TotalAlloc, 2*uint64(len(value)); got > limit {
			t.Errorf("escapeShadow(%q...) allocated %d bytes for %d, limit %d", value[:10], got, len(value), limit)
		}
	}
}

// Raw JSON text is scanned with its escapes intact, so a shadow hit must never split one: eating
// the backslash of \" would close the string early.
func TestEscapeShadowSpansNeverSplitAnEscape(t *testing.T) {
	// Escapes at [2,4) and [10,16).
	_, escapes := escapeShadow(`ab\ncdefgh\` + "u0020ijklmnop")
	for _, c := range []struct{ start, end, wantStart, wantEnd int }{
		{3, 8, 2, 8}, {5, 12, 5, 16}, {2, 4, 2, 4}, {4, 10, 4, 10}, {11, 15, 10, 16}, {0, 20, 0, 20},
	} {
		if s, e := escapes.snap(c.start, c.end); s != c.wantStart || e != c.wantEnd {
			t.Errorf("snap [%d,%d) = [%d,%d), want [%d,%d)", c.start, c.end, s, e, c.wantStart, c.wantEnd)
		}
	}

	// Raw text holding encoded JSON is two levels deep: `\\n` unwinds over two passes into one
	// escape [1,4), and the touching `\\` and `\"` stay one unit [5,9).
	bs := strings.Repeat(`\`, 2)
	value := "a" + bs + "nb" + bs + `\"`
	shadow, got := escapeShadow(value)
	var splits []int
	for k := range len(value) + 1 {
		if hasBit(got.splits, k) {
			splits = append(splits, k)
		}
	}
	if want := []int{2, 3, 6, 7, 8}; !slices.Equal(splits, want) || string(shadow) != "a  \nb \\ \"" {
		t.Errorf("escapeShadow = %q splits %v, want splits %v", shadow, splits, want)
	}

	s := sourcePatchScrubber(t)
	for payload, want := range map[string]string{
		`{"cmd":"cat <<EOF\nAKIAIOSFODNN7EXAMPLE\nEOF"}`:                         `{"cmd":"cat <<EOF\n__REDACTED:aws-access-key-id__\nEOF"}`,
		`{"cmd":"export GITHUB_TOKEN=\"zx81plainvalue\"\necho done\t"}`:          `{"cmd":"export GITHUB_TOKEN=\"__REDACTED:key-name__\"\necho done\t"}`,
		`{"out":"{\"k\":\"x\\nghp_abcdefghijklmnopqrstuvwxyz0123456789\\\\\"}"}`: `{"out":"{\"k\":\"x\\n__REDACTED:github-pat__\\\\\"}"}`,
	} {
		res, err := s.Scrub([]byte(payload), Hint{Family: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		if out := res.Out.Bytes(); string(out) != want || !json.Valid(out) {
			t.Errorf("raw scrub of %s\n got %s\nwant %s", payload, out, want)
		}
	}
}

// One shadow hit bridging two original spans must leave one span covering all three, since
// resolveSpans keeps the first of two overlapping spans and would expose the other's tail.
func TestUnionSpanFoldsEveryOverlap(t *testing.T) {
	base := func() []Span { return []Span{{Start: 0, End: 5, RuleID: "a"}, {Start: 8, End: 12, RuleID: "b"}} }
	for _, c := range []struct {
		add  Span
		want []Span
	}{
		{Span{Start: 3, End: 10, RuleID: "c"}, []Span{{Start: 0, End: 12, RuleID: "a"}}},
		{Span{Start: 1, End: 4, RuleID: "c"}, base()},
		{Span{Start: 6, End: 7, RuleID: "c"}, append(base(), Span{Start: 6, End: 7, RuleID: "c"})},
		{Span{Start: 7, End: 13, RuleID: "c"}, []Span{{Start: 0, End: 5, RuleID: "a"}, {Start: 7, End: 13, RuleID: "c"}}},
	} {
		if got := unionSpan(base(), c.add); !slices.Equal(got, c.want) {
			t.Errorf("union %v = %v, want %v", c.add, got, c.want)
		}
	}
}

// A renamed or dropped rule would silently take a document's key-plus-value check with it.
func TestKeyValueRulesAreCompiled(t *testing.T) {
	var got []string
	for _, p := range sourcePatchScrubber(t).keyValue {
		got = append(got, p.m.(*packs.Rule).RuleID())
	}
	if !slices.Equal(got, keyValueRules) {
		t.Fatalf("compiled key-plus-value rules %v, want %v", got, keyValueRules)
	}
}
