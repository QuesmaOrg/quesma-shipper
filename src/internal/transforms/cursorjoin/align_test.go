package cursorjoin

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// matchBlock's shared candidate ranking relies on text evidence being only positive, neutral or
// negative: the weak grade name-checks ToolFormerData, which text candidates lack, and the
// look-behind drops everything short of positive for prose.
func TestTextEvidenceIsNeverPartialOrWeak(t *testing.T) {
	blocks := []string{"", "Reading the resolver first.", "<user_query>\nlist the workspace\n</user_query>", "unrelated prose"}
	bubbles := map[string]*bubble{
		"user text":         {Type: 1, Text: "list the workspace"},
		"assistant text":    {Type: 2, Text: "Reading the resolver first."},
		"untyped text":      {Text: "Reading the resolver first."},
		"empty text":        {Type: 2},
		"thought":           {Type: 2, IsThought: true, Text: "Reading the resolver first."},
		"thinking":          {Type: 2, Thinking: &thinkingData{Text: "unrelated prose"}},
		"tool":              {Type: 2, ToolFormerData: &toolFormerData{Name: "read_file_v2", RawArgs: `{}`}},
		"tool without name": {Type: 2, ToolFormerData: &toolFormerData{}},
	}
	for _, text := range blocks {
		for name, b := range bubbles {
			for _, role := range []string{"user", "assistant"} {
				blk := block{Type: "text", Text: flexString(text)}
				desc := fmt.Sprintf("%q vs %s as %s", text, name, role)

				// A lone candidate at the cursor surfaces its grade: a partial wins outright, a weak one is the last resort.
				_, outcome, ev, _ := matchBlock(blk, role, []*bubble{b}, 0, make([]bubbleState, 1))
				if outcome != matchNone {
					assert.Equal(t, matchFound, outcome, desc)
					assert.Contains(t, []evidence{evidencePositive, evidenceNeutral}, ev, desc)
				}

				// Behind the cursor, prose matches only on real overlap.
				_, outcome, ev, _ = matchBlock(blk, role, []*bubble{b}, 1, make([]bubbleState, 1))
				if outcome != matchNone {
					assert.Equal(t, matchFound, outcome, desc)
					assert.Equal(t, evidencePositive, ev, desc)
				}
			}
		}
	}
}
