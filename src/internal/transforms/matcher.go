package transforms

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Span is a byte range inside one decoded string value, attributed to the rule that
// matched it.
type Span struct {
	Start  int
	End    int
	RuleID string
}

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

// prioritizedSpan carries the matcher class alongside the span, so overlapping
// matches resolve by confidence rather than alphabetically.
type prioritizedSpan struct {
	Span
	// priority 0 is a high-confidence pattern rule, 1 is a heuristic. Lower wins.
	priority int
}

// resolveSpans merges overlapping spans into one placeholder rather than nesting them.
// The surviving attribution must be the HIGHEST-CONFIDENCE rule covering the region: the
// entropy backstop fires on nearly every provider key too, so any other tie-break turns
// the ledger into "something high-entropy happened".
func resolveSpans(value string, patternSpans, heuristicSpans []Span) ([]Span, int, map[string]int) {
	if len(patternSpans) == 0 && len(heuristicSpans) == 0 {
		return nil, 0, nil
	}

	spans := make([]prioritizedSpan, 0, len(patternSpans)+len(heuristicSpans))
	for _, s := range patternSpans {
		spans = append(spans, prioritizedSpan{Span: s, priority: 0})
	}
	for _, s := range heuristicSpans {
		spans = append(spans, prioritizedSpan{Span: s, priority: 1})
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

	// Keeping both would nest placeholders or split one secret across two.
	spans = dropOverlappedHeuristics(spans)

	resolved := make([]Span, 0, len(spans))
	hits := map[string]int{}
	redacted := 0
	cursor := 0

	for _, s := range spans {
		if s.Start < 0 || s.End > len(value) || s.Start >= s.End {
			continue
		}
		if s.Start < cursor {
			// Already inside a replaced region.
			continue
		}
		resolved = append(resolved, s.Span)
		hits[s.RuleID]++
		redacted += s.End - s.Start
		cursor = s.End
	}
	return resolved, redacted, hits
}

// dropOverlappedHeuristics removes heuristic spans intersecting a pattern span and widens
// that span over any reach past it, so one secret yields one confidently attributed
// placeholder.
func dropOverlappedHeuristics(spans []prioritizedSpan) []prioritizedSpan {
	var patterns []prioritizedSpan
	for _, s := range spans {
		if s.priority == 0 {
			patterns = append(patterns, s)
		}
	}

	out := make([]prioritizedSpan, 0, len(spans))
	for _, s := range spans {
		if s.priority == 0 {
			out = append(out, s)
			continue
		}
		overlapped := false
		for i, p := range patterns {
			if s.Start < p.End && p.Start < s.End {
				// Extend the confident span so no tail of the secret escapes.
				if s.End > patterns[i].End {
					patterns[i].End = s.End
					for j := range out {
						if out[j].priority == 0 && out[j].Start == p.Start && out[j].RuleID == p.RuleID {
							out[j].End = s.End
						}
					}
				}
				overlapped = true
				break
			}
		}
		if !overlapped {
			out = append(out, s)
		}
	}
	return out
}

// EngineError marks a failure of the scrubber itself. on_error: fail_closed fires on
// these ONLY, so a file that hits one does not upload; a payload that merely failed to
// parse is raw-text scanned and ships byte-exact.
type EngineError struct {
	Op  string
	Err error
}

func (e *EngineError) Error() string { return fmt.Sprintf("scrub engine: %s: %v", e.Op, e.Err) }
func (e *EngineError) Unwrap() error { return e.Err }
