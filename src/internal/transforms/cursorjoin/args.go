// args.go weighs a transcript tool_use against a store tool call's recorded arguments. The
// join's most drift-exposed surface: every store generation so far has changed how arguments are
// recorded, so each rule names the observed behaviour that forced it.

package cursorjoin

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// evidence is what a tool bubble's recorded arguments say about a transcript tool_use. The
// order is the candidate ranking: when no bubble's arguments agree in full, matchBlock takes
// the highest grade in reach before falling back to position.
type evidence int

const (
	// The two sides recorded comparable values and none of them agree.
	evidenceNegative evidence = iota
	// Some values agree, none long enough to rule out coincidence — two searches of the same
	// tree share their path and nothing else. Ranks below an empty record: a bubble whose
	// values half-belong to another call has identified itself as probably that call's.
	evidenceWeak
	// One side recorded nothing able to confirm or deny — current stores write rawArgs "{}"
	// for most terminal commands. Usable only as a positional fallback.
	evidenceNeutral
	// A whole value too long to be an accident agrees while another goes unaccounted: the
	// store recording variants of what the transcript holds. Not identity, but not silence.
	evidencePartial
	// Every comparable value agrees, at least one of them substantial.
	evidencePositive
)

// argsEvidence weighs a transcript tool_use against a store tool call's recorded arguments.
// Alongside the grade it returns the strength — how many values agreed — because two bubbles can
// both grade positive and the one agreeing on more of the call is the call.
func argsEvidence(input json.RawMessage, t *toolFormerData) (evidence, int) {
	if len(input) == 0 {
		return evidenceNeutral, 0
	}
	stored := strings.TrimSpace(t.RawArgs)
	if stored == "" || stored == "{}" || stored == "null" {
		stored = strings.TrimSpace(t.Params)
	}
	if stored == "" || stored == "{}" || stored == "null" {
		// Current stores write rawArgs "{}" with empty params for most terminal commands.
		return evidenceNeutral, 0
	}

	// Whole value against whole value, both sides taken apart first, rather than value against
	// the stored JSON text: the two sides name the same argument differently
	// (glob_pattern/globPattern), so the keys cannot be paired up, and substring evidence
	// matches unrelated calls — a glob of `**/*` sits inside `**/*.{md,go}`, a directory inside
	// every path under it.
	storedVals, parsed := argValues(stored)
	if parsed && !slices.ContainsFunc(storedVals, func(sv argValue) bool { return !sv.weak }) {
		// Nothing that could confirm a call, as errored calls are recorded. Position may
		// still speak for the bubble.
		return evidenceNeutral, 0
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		// The input is not an object; containment is all that is left.
		if !parsed && strings.Contains(stripCursorAttribution(stored), string(input)) {
			return evidencePositive, 1
		}
		return evidenceNegative, 0
	}

	storedSet := make(map[string]bool, len(storedVals))
	for _, sv := range storedVals {
		storedSet[sv.v] = true
	}
	// Attribution stripped up front: the transcript holds the model's intent, the store what
	// actually ran, and an executed form scoring as contradiction bars the call's own bubble.
	strippedStored := ""
	if !parsed {
		strippedStored = stripCursorAttribution(stored)
	}

	strongComparable, strongMatched := 0, 0
	long := false
	weakUnaccounted := false
	for _, av := range collectArgValues(m, genericArgKeys, nil) {
		if av.weak {
			// Too short to confirm anything, but still separates two calls when it
			// DISAGREES. Only against a parsed record: an unparsed one cannot be
			// searched for a three-character token safely.
			if parsed && !storedSet[av.v] {
				weakUnaccounted = true
			}
			continue
		}
		strongComparable++
		if storedSet[av.v] {
			strongMatched++
			long = long || len(av.v) >= longArgLen
			continue
		}
		if slices.ContainsFunc(storedVals, func(sv argValue) bool {
			return !sv.weak && stripCursorAttribution(sv.v) == av.v
		}) {
			strongMatched++
			long = long || len(av.v) >= longArgLen
			continue
		}
		if !parsed && len(av.v) >= longArgLen {
			// Older generations wrote a bare string, leaving only containment, gated to a
			// length that cannot collide — and against the escaped form too, since a value
			// with quotes or newlines appears escaped inside a string field holding JSON.
			if strings.Contains(strippedStored, av.v) {
				strongMatched++
				long = true
				continue
			}
			if e := jsonEscaped(av.v); e != av.v && strings.Contains(strippedStored, e) {
				strongMatched++
				long = true
				continue
			}
		}
	}
	switch {
	case strongComparable == 0:
		// Nothing on the transcript side substantial enough to compare either way.
		return evidenceNeutral, 0
	case strongMatched == strongComparable && !weakUnaccounted:
		// Identity is agreement on everything comparable, not a majority: two greps sharing
		// path and glob but not pattern swapped each other's results with no alarm. One
		// disagreeing value, however short, vetoes.
		return evidencePositive, strongMatched
	case !parsed && strongMatched > 0:
		// Containment already gates to values too long to collide.
		return evidencePositive, strongMatched
	case strongMatched > 0 && long:
		return evidencePartial, strongMatched
	case strongMatched > 0:
		// Not contradiction, measured against the live store: a true bubble routinely
		// matches a strict subset of its block's values, because the store records variants
		// the transcript does not — edit_file_v2 records the path and never the strings.
		// Scoring these negative barred nine real conversations' own bubbles.
		return evidenceWeak, strongMatched
	default:
		return evidenceNegative, 0
	}
}

// The fragments Cursor splices into a command between the transcript's record and the store's,
// observed live 2026-08-19. The literals are the vendor's, so their drift lands as a mismatch
// alarm. The trailer carries its leading space so it strips wherever the flag sits.
var cursorAttributions = []string{
	` --trailer "Co-authored-by: Cursor <cursoragent@cursor.com>"`,
	"\n\nMade with [Cursor](https://cursor.com)",
}

// stripCursorAttribution removes the splices in both the plain and the JSON-escaped form: inside
// a stored blob holding JSON as a string, the plain literal never occurs.
func stripCursorAttribution(s string) string {
	for _, a := range cursorAttributions {
		s = strings.ReplaceAll(s, a, "")
		if e := jsonEscaped(a); e != a {
			s = strings.ReplaceAll(s, e, "")
		}
	}
	return s
}

// minArgLen is the shortest value that can CONFIRM anything, longArgLen the shortest a
// CONTAINMENT match may: `**/*`, `*.go` and `-la` are arguments of a hundred unrelated calls.
const (
	minArgLen  = 4
	longArgLen = 32
)

// argValue is one comparable argument value in canonical string form; weak marks the ones that
// can contradict identity but never confirm it.
type argValue struct {
	v    string
	weak bool
}

// argValues pulls the comparable values out of a stored argument blob, at any depth, and reports
// whether it could be taken apart at all. False means the caller has nothing but the raw text.
func argValues(stored string) ([]argValue, bool) {
	var v any
	if err := json.Unmarshal([]byte(stored), &v); err != nil {
		return nil, false
	}
	return collectArgValues(v, storedNoiseKeys, nil), true
}

// collectArgValues walks one side's decoded arguments and appends every comparable value, in a
// deterministic order. Both sides go through this one walk, to the same depth. Numbers are weak
// like short strings — offsets and limits recur across unrelated calls — and booleans are no
// evidence at all: a flag the store never records demoted the true bubble on most terminal calls.
func collectArgValues(v any, skip map[string]bool, out []argValue) []argValue {
	switch t := v.(type) {
	case string:
		if t != "" {
			out = append(out, argValue{v: t, weak: len(t) < minArgLen})
		}
	case float64:
		out = append(out, argValue{v: strconv.FormatFloat(t, 'g', -1, 64), weak: true})
	case []any:
		for _, e := range t {
			out = collectArgValues(e, skip, out)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if skip[k] {
				continue
			}
			out = collectArgValues(t[k], skip, out)
		}
	}
	return out
}

// Stored argument keys the transcript has no counterpart for, so a value under one can only match
// by accident. parsingResult is a parse tree of the terminal command — every shell token its own
// string, plus the workspace root — and any fragment of it let an unrelated block steal the
// terminal bubble.
var storedNoiseKeys = map[string]bool{
	"toolCallId":             true,
	"parsingResult":          true,
	"requestedSandboxPolicy": true,
	"commandDescription":     true,
	"cwd":                    true,
}

// genericArgKeys are transcript input keys whose values recur across unrelated calls and so
// cannot discriminate between bubbles.
var genericArgKeys = map[string]bool{
	"working_directory": true,
	"cwd":               true,
	"description":       true,
	"explanation":       true,
}

// namesCompatible reports whether a transcript display name and a store internal name plausibly
// denote the same tool. Consulted only for the positional fallbacks: a mapping this coarse must
// never override argument evidence.
func namesCompatible(display string, t *toolFormerData) bool {
	internal := cmp.Or(t.Name, string(t.Tool))
	if display == "" || internal == "" {
		return true // nothing to compare; position stands
	}
	d := normaliseToolName(display)
	in := normaliseToolName(internal)
	if d == "" || in == "" || d == in || strings.Contains(in, d) || strings.Contains(d, in) {
		return true
	}
	// MCP calls: the transcript names the generic dispatcher, the store the concrete tool it
	// routed to, so the substring rule never relates the two. Observed live 2026-08-20.
	if d == "callmcptool" && strings.HasPrefix(in, "mcp") {
		return true
	}
	pairs := map[string][]string{
		"shell":      {"runterminalcommandv2", "runterminalcmd"},
		"strreplace": {"editfilev2", "searchreplace"},
		"write":      {"editfilev2", "writefile", "createfilev2"},
	}
	for _, want := range pairs[d] {
		if in == want {
			return true
		}
	}
	return false
}

func normaliseToolName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// jsonEscaped is the string as it appears inside a JSON document, without the quotes.
func jsonEscaped(s string) string {
	b, err := marshalCompact(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}
