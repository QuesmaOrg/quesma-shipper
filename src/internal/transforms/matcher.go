package transforms

import (
	"cmp"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// Span is a byte range inside one decoded string value, attributed to the rule that
// matched it; the packs type, so rule matchers and the engine share one span.
type Span = packs.Span

// FieldPath is a dotted path to a value inside a record, with "[]" standing for array
// elements: content[].image.hex, message.content[].text, toolUseResult.stdout.
type FieldPath string

// Sentinel is the placeholder for a redacted value. Its width depends only on the rule id,
// never on the secret, and nothing in it may ever derive from the secret's value: a hash
// prefix is reversible redaction. The rule id is what makes the ledger queryable.
func Sentinel(ruleID string) string {
	return sentinelPrefix + ":" + ruleID + "__"
}

// sentinelPrefix is the run-visible head of every sentinel: the ":" after it is outside the
// entropy candidate alphabet, so only this part fuses with adjacent text. The entropy matcher
// must keep skipping candidates carrying it, or a re-scrub eats the previous pass's ledger.
const sentinelPrefix = "__REDACTED"

// isSentinel reports whether s is exactly one sentinel.
func isSentinel(s string) bool {
	id, ok := strings.CutPrefix(s, sentinelPrefix+":")
	if !ok {
		return false
	}
	if id, ok = strings.CutSuffix(id, "__"); !ok || id == "" {
		return false
	}
	for i := range len(id) {
		if c := id[i]; c != '-' && !isLower(c) && !isDigit(c) {
			return false
		}
	}
	return true
}

// ExemptionSet holds the structural exemptions in force: identifier fields whose redaction
// would destroy the causal graph, and declared opaque binary payloads. Heuristics only, and
// paths match exactly rather than by key name, since additions here weaken scrubbing.
type ExemptionSet struct {
	byFamily map[string]map[FieldPath]bool
	global   map[FieldPath]bool
}

// NewExemptionSet builds the set from the resolved config's structural_exempt map, keyed
// by source family or "*" for all.
func NewExemptionSet(spec map[string][]string) *ExemptionSet {
	e := &ExemptionSet{
		byFamily: map[string]map[FieldPath]bool{},
		global:   map[FieldPath]bool{},
	}
	for family, paths := range spec {
		if family == "*" {
			for _, p := range paths {
				e.global[FieldPath(p)] = true
			}
			continue
		}
		if e.byFamily[family] == nil {
			e.byFamily[family] = map[FieldPath]bool{}
		}
		for _, p := range paths {
			e.byFamily[family][FieldPath(p)] = true
		}
	}
	return e
}

// Exempt reports whether a field is exempt from heuristic detectors for a family. No
// nil-receiver tolerance: New always builds the set, and answering from a nil one would
// fail open.
func (e *ExemptionSet) Exempt(family string, field FieldPath) bool {
	if e.global[field] {
		return true
	}
	return e.byFamily[family][field]
}

// ExemptKeys reports whether a "<parent>.*" entry covers the keys of the object at parent:
// a map keyed by ids has no fixed path to name. It covers the keys only, never their values.
func (e *ExemptionSet) ExemptKeys(family string, parent FieldPath) bool {
	return e.Exempt(family, FieldPath(joinFieldPath(string(parent), "*")))
}

// prioritizedSpan carries the matcher class alongside the span, so overlapping
// matches resolve by confidence rather than alphabetically.
type prioritizedSpan struct {
	Span
	// priority 0 is a high-confidence pattern rule, 1 is a heuristic. Lower wins.
	priority int
}

// resolveSpans joins overlapping spans into one placeholder rather than nesting them or keeping
// one: a kept span beside a dropped overlapping one ships the dropped one's tail in the clear.
// The region's attribution is the HIGHEST-CONFIDENCE rule covering it: the entropy backstop fires
// on nearly every provider key too, so any other tie-break turns the ledger into "something
// high-entropy happened".
func resolveSpans(value string, patternSpans, heuristicSpans []Span) ([]Span, int, map[string]int) {
	valid := func(s Span) bool { return s.Start >= 0 && s.End <= len(value) && s.Start < s.End }
	spans := make([]prioritizedSpan, 0, len(patternSpans)+len(heuristicSpans))
	for _, s := range patternSpans {
		if valid(s) {
			spans = append(spans, prioritizedSpan{Span: s, priority: 0})
		}
	}
	patterns := spans
	for _, s := range heuristicSpans {
		if valid(s) {
			s.Start = max(s.Start, firstPatternStart(s, patterns))
			spans = append(spans, prioritizedSpan{Span: s, priority: 1})
		}
	}
	if len(spans) == 0 {
		return nil, 0, nil
	}

	slices.SortFunc(spans, func(a, b prioritizedSpan) int {
		if a.Start != b.Start {
			return cmp.Compare(a.Start, b.Start)
		}
		if a.priority != b.priority {
			return cmp.Compare(a.priority, b.priority)
		}
		if a.End != b.End {
			return cmp.Compare(b.End, a.End)
		}
		return strings.Compare(a.RuleID, b.RuleID)
	})

	// Joined in place: the regions never outgrow the spans already read, and a large value
	// carries thousands of spans.
	regions := spans[:1]
	for _, s := range spans[1:] {
		r := &regions[len(regions)-1]
		if s.Start >= r.End {
			regions = append(regions, s)
			continue
		}
		r.End = max(r.End, s.End)
		if s.priority < r.priority {
			r.RuleID, r.priority = s.RuleID, s.priority
		}
	}

	resolved := make([]Span, 0, len(regions))
	hits := map[string]int{}
	redacted := 0
	for _, r := range regions {
		resolved = append(resolved, r.Span)
		hits[r.RuleID]++
		redacted += r.End - r.Start
	}
	return resolved, redacted, hits
}

// firstPatternStart is where the earliest pattern span overlapping h starts, or -1 if none does.
// A pattern's start is exact (the key-name rule leaves the name visible), so an entropy run
// reaching back before it keeps only what lies from that start on. Heuristic spans must be
// disjoint, as the entropy matcher's maximal runs are: two overlapping runs would each be trimmed
// against the patterns alone, and the bytes between could ship.
func firstPatternStart(h Span, patterns []prioritizedSpan) int {
	first := -1
	for _, p := range patterns {
		if p.Start < h.End && h.Start < p.End && (first < 0 || p.Start < first) {
			first = p.Start
		}
	}
	return first
}
