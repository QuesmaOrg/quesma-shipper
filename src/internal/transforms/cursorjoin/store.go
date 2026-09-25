// store.go is the store side of the join: the vendor row shapes in the two generations a live
// state.vscdb holds, and the indexing and ordering that turn rows into a conversation's event
// list. Every struct decodes only what the join reads: no rows ship.

package cursorjoin

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// composerData is the per-conversation record. Only the fields the join needs are decoded:
// rows reach megabytes, and decoding whole would pull attachment hex into memory for nothing.
type composerData struct {
	// The newer generation's authoritative bubble order, which the inline array does not carry.
	FullConversationHeadersOnly []header `json:"fullConversationHeadersOnly"`

	// The older generation's inline array, empty on newer conversations.
	Conversation []bubble `json:"conversation"`
}

type header struct {
	BubbleID string `json:"bubbleId"`
}

// bubble is one turn record.
type bubble struct {
	BubbleID  string `json:"bubbleId"`
	Type      int    `json:"type"` // 1 = user, 2 = assistant
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	RequestID string `json:"requestId"`

	// Scaffolding markers, skipped as non-events. Current stores stamp capabilityType 15 on
	// every tool bubble, so the flag alone no longer implies scaffolding; see isScaffolding.
	IsCapabilityIteration bool       `json:"isCapabilityIteration"`
	CapabilityType        flexString `json:"capabilityType"`

	// Older stores mark reasoning with isThought, current ones write a thinking object; current
	// transcripts write it as a plain text block, so it is alignable. See matchBlock.
	IsThought bool          `json:"isThought"`
	Thinking  *thinkingData `json:"thinking"`

	ToolFormerData *toolFormerData `json:"toolFormerData"`

	CheckpointID   string `json:"checkpointId"`
	TurnDurationMs int64  `json:"turnDurationMs"`

	// ModelName is the older stores' field; current ones nest it as modelInfo.modelName.
	ModelName string     `json:"modelName"`
	ModelInfo *modelInfo `json:"modelInfo"`
}

type modelInfo struct {
	ModelName string `json:"modelName"`
}

// thinkingData decodes reasoning in both encodings a store holds: an object, and that object
// serialised into a string on server-hydrated rows. A strict field would reject the whole row.
type thinkingData struct {
	Text string `json:"text"`
}

func (t *thinkingData) UnmarshalJSON(b []byte) error {
	// A named type, so decoding the object form does not re-enter this method.
	type plain thinkingData
	var s string
	if json.Unmarshal(b, &s) != nil {
		return json.Unmarshal(b, (*plain)(t))
	}
	// A string with no text under the known name is taken as the reasoning itself.
	if json.Unmarshal([]byte(s), (*plain)(t)) != nil || t.Text == "" {
		t.Text = s
	}
	return nil
}

// reasoningText is a bubble's reasoning in either generation's encoding; empty means none.
func (b *bubble) reasoningText() string {
	if b.Thinking != nil && b.Thinking.Text != "" {
		return b.Thinking.Text
	}
	if b.IsThought {
		return b.Text
	}
	return ""
}

// flexString decodes any JSON value as a string: the store migrated several fields to numeric
// enums, and a strict string field makes json.Unmarshal reject the whole bubble row.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	// A non-string keeps its raw JSON text rather than costing the record it sits in.
	if json.Unmarshal(b, (*string)(f)) != nil {
		*f = flexString(b)
	}
	return nil
}

// toolFormerData is the tool call as the store records it: the id, the status, and the result
// this whole enricher exists for. Tool is a string in older stores, a numeric enum in current.
type toolFormerData struct {
	ToolCallID string     `json:"toolCallId"`
	Name       string     `json:"name"`
	Tool       flexString `json:"tool"`
	Status     string     `json:"status"`
	RawArgs    string     `json:"rawArgs"`
	Params     string     `json:"params"`
	Result     string     `json:"result"`
}

// indexed is the store side, arranged for the join.
type indexed struct {
	composers map[string]*composerData
	bubbles   map[string]map[string]*bubble // conversation -> bubbleId -> bubble
	order     map[string][]string           // conversation -> bubble ids in key order

	// Surfaced as a note, never swallowed: silence here turns struct drift into a mismatch
	// alarm that points away from its cause.
	decodeErrors int
}

// indexRows decodes only the staged conversations' rows: the keyspaces hold a machine's whole
// Cursor history, and the join reads nothing else.
func indexRows(rows []sqliteread.Row, staged map[string]bool) *indexed {
	ix := &indexed{
		composers: map[string]*composerData{},
		bubbles:   map[string]map[string]*bubble{},
		order:     map[string][]string{},
	}
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.Key, composerPrefix):
			conv := strings.TrimPrefix(r.Key, composerPrefix)
			if !staged[conv] {
				continue
			}
			var c composerData
			if err := json.Unmarshal(r.Value, &c); err != nil {
				ix.decodeErrors++
				continue
			}
			ix.composers[conv] = &c

		case strings.HasPrefix(r.Key, bubblePrefix):
			// bubbleId:<conversation>:<bubble>
			conv, bubbleID, ok := strings.Cut(strings.TrimPrefix(r.Key, bubblePrefix), ":")
			if !staged[conv] {
				continue
			}
			if !ok {
				ix.decodeErrors++
				continue
			}
			var b bubble
			if err := json.Unmarshal(r.Value, &b); err != nil {
				ix.decodeErrors++
				continue
			}
			if b.BubbleID == "" {
				b.BubbleID = bubbleID
			}
			if ix.bubbles[conv] == nil {
				ix.bubbles[conv] = map[string]*bubble{}
			}
			ix.bubbles[conv][bubbleID] = &b
			ix.order[conv] = append(ix.order[conv], bubbleID)
		}
	}
	return ix
}

// orderBubbles produces the authoritative bubble order: fullConversationHeadersOnly, the only
// place it is recorded, then a key scan for the errored turns whose header list is empty.
func orderBubbles(c *composerData, bubbles map[string]*bubble, keyOrder []string) []*bubble {
	if c != nil && len(c.FullConversationHeadersOnly) > 0 {
		out := make([]*bubble, 0, len(c.FullConversationHeadersOnly))
		for _, h := range c.FullConversationHeadersOnly {
			// A header naming an absent row is normal after compaction; skipping the gap
			// rather than counting it is what keeps a compacted conversation shipping.
			if b, ok := bubbles[h.BubbleID]; ok {
				out = append(out, b)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	// Inline conversation, the older generation.
	if c != nil && len(c.Conversation) > 0 {
		out := make([]*bubble, 0, len(c.Conversation))
		for i := range c.Conversation {
			out = append(out, &c.Conversation[i])
		}
		return out
	}

	// Key scan. createdAt is a real timestamp, unlike anything in the transcript, so it is the
	// ordering of last resort; ties fall back to the sorted key order, so this is deterministic.
	out := make([]*bubble, 0, len(bubbles))
	seen := map[string]bool{}
	for _, id := range keyOrder {
		if b, ok := bubbles[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, b)
		}
	}
	slices.SortStableFunc(out, func(a, b *bubble) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	return out
}

// isScaffolding reports whether a bubble has no transcript counterpart by design.
func isScaffolding(b *bubble) bool {
	// A tool call or a reasoning bubble is an event whatever else it is flagged with; one the
	// transcript does not mention simply goes unconsumed, which alignment tolerates.
	if b.ToolFormerData != nil {
		return false
	}
	if b.reasoningText() != "" {
		return false
	}
	if b.IsCapabilityIteration || b.CapabilityType != "" {
		return true
	}
	// Neither text nor a tool call: an artefact of how the store records turn boundaries.
	return b.Text == ""
}
