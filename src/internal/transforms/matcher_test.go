package transforms

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// A span dropped beside an overlapping one shipped its tail; only a pattern's exact start trims a run.
func TestResolveSpansJoinsOverlaps(t *testing.T) {
	value := strings.Repeat("x", 100)
	for _, c := range []struct {
		name              string
		patterns, entropy []Span
		want              []Span
		wantHits          map[string]int
	}{
		{
			name:     "a run starting before a pattern keeps its reach past it",
			patterns: []Span{{Start: 10, End: 20, RuleID: "p"}},
			entropy:  []Span{{Start: 0, End: 28, RuleID: "e"}},
			want:     []Span{{Start: 10, End: 28, RuleID: "p"}},
			wantHits: map[string]int{"p": 1},
		},
		{
			name:     "a run starting inside a pattern widens it",
			patterns: []Span{{Start: 0, End: 20, RuleID: "p"}},
			entropy:  []Span{{Start: 5, End: 28, RuleID: "e"}},
			want:     []Span{{Start: 0, End: 28, RuleID: "p"}},
			wantHits: map[string]int{"p": 1},
		},
		{
			name:     "a run over two patterns joins them from the first one's start",
			patterns: []Span{{Start: 30, End: 40, RuleID: "q"}, {Start: 10, End: 20, RuleID: "p"}},
			entropy:  []Span{{Start: 0, End: 50, RuleID: "e"}},
			want:     []Span{{Start: 10, End: 50, RuleID: "p"}},
			wantHits: map[string]int{"p": 1},
		},
		{
			name:     "overlapping patterns join under the earlier one",
			patterns: []Span{{Start: 0, End: 10, RuleID: "p"}, {Start: 5, End: 20, RuleID: "q"}},
			want:     []Span{{Start: 0, End: 20, RuleID: "p"}},
			wantHits: map[string]int{"p": 1},
		},
		{
			name:     "overlapping runs join",
			entropy:  []Span{{Start: 0, End: 10, RuleID: "e"}, {Start: 5, End: 20, RuleID: "e"}},
			want:     []Span{{Start: 0, End: 20, RuleID: "e"}},
			wantHits: map[string]int{"e": 1},
		},
		{
			name:     "touching spans stay two placeholders",
			patterns: []Span{{Start: 0, End: 10, RuleID: "p"}, {Start: 10, End: 20, RuleID: "q"}},
			want:     []Span{{Start: 0, End: 10, RuleID: "p"}, {Start: 10, End: 20, RuleID: "q"}},
			wantHits: map[string]int{"p": 1, "q": 1},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, redacted, hits := resolveSpans(value, c.patterns, c.entropy)
			if !slices.Equal(got, c.want) {
				t.Errorf("spans %v, want %v", got, c.want)
			}
			wantRedacted := 0
			for _, s := range c.want {
				wantRedacted += s.End - s.Start
			}
			if redacted != wantRedacted {
				t.Errorf("redacted %d bytes, want %d", redacted, wantRedacted)
			}
			if !maps.Equal(hits, c.wantHits) {
				t.Errorf("hits %v, want %v", hits, c.wantHits)
			}
		})
	}
}
