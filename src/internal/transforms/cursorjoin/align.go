// align.go walks the transcript and the ordered bubbles together and renders the derived
// object: the alignment cursor, the tail and repeat exceptions, and the byte-preserving output
// encoding.

package cursorjoin

import (
	"bytes"
	"cmp"
	"encoding/json"
	"strings"
)

// line is one JSONL record, decoded only as far as the join needs. Scalars are flexString for
// the same reason as on the store side: a drifted field shape would reject the whole line, which
// then ships as native_invalid with its bubbles skipped and mismatches at zero — silent loss.
type line struct {
	Role    flexString `json:"role"`
	Message *message   `json:"message"`
}

type message struct {
	Content []block `json:"content"`
}

type block struct {
	Type  flexString      `json:"type"`
	Text  flexString      `json:"text"`
	Name  flexString      `json:"name"`
	Input json.RawMessage `json:"input"`
}

// outLine is one line of the derived JSONL. Native holds the original line's bytes verbatim, so
// no part of the native record depends on this code. Enrich is an array index-aligned to the
// native content blocks, never a map: a map's key order is lexicographic, so block 10 would sort
// before block 2 and the output bytes would stop being a stable function of the input.
type outLine struct {
	Native json.RawMessage `json:"native,omitempty"`

	// A line that is not valid JSON, carried as a string rather than dropped: a torn tail is
	// expected, and an invalid raw value would make the whole derived document unmarshalable.
	// Not byte-exact for a mid-rune tear, whose dangling bytes become U+FFFD — the byte-exact
	// record is the raw transcript, which ships anyway.
	NativeInvalid string `json:"native_invalid,omitempty"`

	Enrich []*blockEnrich `json:"_enrich,omitempty"`
}

// blockEnrich is what the store knew and the transcript did not.
type blockEnrich struct {
	BubbleID string `json:"bubbleId,omitempty"`

	// The correlation key the transcript is missing entirely.
	ToolCallID string `json:"tool_call_id,omitempty"`

	// ToolName is the INTERNAL name, which can differ from the transcript's display name.
	ToolName string `json:"tool_name,omitempty"`
	Status   string `json:"status,omitempty"`

	// The full tool output, which the transcript has none of.
	Result string `json:"result,omitempty"`

	CreatedAt      string `json:"createdAt,omitempty"`
	RequestID      string `json:"requestId,omitempty"`
	CheckpointID   string `json:"checkpointId,omitempty"`
	TurnDurationMs int64  `json:"turnDurationMs,omitempty"`
	ModelName      string `json:"modelName,omitempty"`
}

// alignAndRender walks the transcript and the ordered bubbles together, one pass over each,
// advancing the bubble cursor only on a match. The transcript is the authority on what happened,
// so an event it contains that the store cannot account for is a mismatch, while a store bubble
// the transcript does not mention is simply skipped. Three exceptions ship native-only and are
// counted apart: tail (the store ends before the transcript, as with injected turns), repeats
// (one bubble for a call the agent ran twice) and ambiguous (see matchAmbiguous).
func alignAndRender(content []byte, events []*bubble) (alignment, error) {
	var out bytes.Buffer
	cursor := 0

	// Unmatched blocks are classified once the pass completes, by where the last CONSUMING
	// match landed. Repeats and ambiguous events move no watermark, and a hole before one is
	// still caught by any real match after it.
	seq := 0
	lastMatchedSeq := -1
	var unmatched []int
	repeats := 0
	ambiguous := 0

	// used marks consumed bubbles individually, because a match may land behind the cursor:
	// within one turn the store's header order and the transcript's block order can disagree.
	// consumedEv and consumedN record the evidence each consumption was made on, which is what
	// repeat detection compares a later claimant against. declined bars bubbles an ambiguous
	// verdict refused to decide about from every later fallback.
	used := make([]bool, len(events))
	consumedEv := make([]evidence, len(events))
	consumedN := make([]int, len(events))
	declined := make([]bool, len(events))

	// Decode failures are positioned, not just counted: only the final line's can be the
	// expected torn tail, and a mid-file one loses its blocks' enrichment with mismatches at zero.
	lineNo := 0
	invalidLines := 0
	lastInvalidLine := -1

	for raw := range bytes.SplitSeq(content, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		lineNo++

		var l line
		if err := json.Unmarshal(trimmed, &l); err != nil {
			// A truncated tail is expected: Cursor's transcript writes are not atomic.
			invalidLines++
			lastInvalidLine = lineNo
			if err := writeLine(&out, outLine{NativeInvalid: string(trimmed)}); err != nil {
				return alignment{}, err
			}
			continue
		}

		// turn_ended is a terminator, not an event. It has no bubble and must not consume
		// the cursor.
		if l.Role == "" || l.Message == nil || len(l.Message.Content) == 0 {
			if err := writeLine(&out, outLine{Native: append(json.RawMessage{}, trimmed...)}); err != nil {
				return alignment{}, err
			}
			continue
		}

		enriched := make([]*blockEnrich, len(l.Message.Content))
		matchedAny := false
		for i, blk := range l.Message.Content {
			// Reasoning arrives as a literal [REDACTED] with nothing to align to.
			if blk.Type == "text" && isRedactedReasoning(string(blk.Text)) {
				continue
			}
			idx, outcome, ev, n := matchBlock(blk, string(l.Role), events, cursor, used, consumedEv, consumedN, declined)
			seq++
			switch outcome {
			case matchNone:
				unmatched = append(unmatched, seq)
				continue
			case matchRepeat:
				// Explained, but nothing to attach: the consumed bubble's result
				// belongs to the run that consumed it.
				repeats++
				continue
			case matchAmbiguous:
				ambiguous++
				declined[idx] = true
				continue
			}
			lastMatchedSeq = seq
			used[idx] = true
			consumedEv[idx], consumedN[idx] = ev, n
			if idx+1 > cursor {
				cursor = idx + 1
			}
			matchedAny = true
			enriched[i] = fromBubble(events[idx])
		}
		if !matchedAny {
			enriched = nil
		}
		if err := writeLine(&out, outLine{
			Native: append(json.RawMessage{}, trimmed...),
			Enrich: enriched,
		}); err != nil {
			return alignment{}, err
		}
	}

	a := alignment{out: out.Bytes(), repeats: repeats, ambiguous: ambiguous}
	// Exempt by position, not by kind: a torn tail is by definition terminal.
	a.lineDecodeErrors = invalidLines
	if invalidLines > 0 && lastInvalidLine == lineNo {
		a.lineDecodeErrors--
	}
	for _, s := range unmatched {
		// A transcript that matched nothing is all mismatch, not all tail.
		if lastMatchedSeq >= 0 && s > lastMatchedSeq {
			a.tail++
		} else {
			a.mismatches++
		}
	}
	return a, nil
}

// alignment is one conversation's alignment outcome: the rendered lines, the events nothing
// accounts for, and the explained shortfalls that ship native-only.
type alignment struct {
	out        []byte
	mismatches int
	tail       int
	repeats    int

	// Tool blocks the evidence could not decide; nothing is attached to them.
	ambiguous int

	// Transcript lines that did not decode, excluding the final line's vendor-confirmed torn
	// tail. Surfaced by the caller the way indexed.decodeErrors is.
	lineDecodeErrors int
}

// How far from the cursor matchBlock will look for a bubble to CONSUME. Evidence decides between
// nearby bubbles; unbounded, one coincidental match drags the cursor past every real bubble
// behind it, each of those then a mismatch. Repeat detection is exempt: it consumes nothing, and
// a deduped re-run's original can sit a whole subagent back.
const (
	lookAhead  = 16
	lookBehind = 64
)

// matchOutcome is what matchBlock concluded about one content block.
type matchOutcome int

const (
	// No bubble accounts for the block; the caller classifies it as mismatch or tail.
	matchNone matchOutcome = iota
	// The returned index is the block's bubble.
	matchFound
	// The block relates to a bubble an earlier event consumed exactly as that consumer did:
	// the store recorded one bubble for a call the agent ran more than once.
	matchRepeat
	// The evidence tells two stories at once — a deduped re-run, or a second run the store
	// recorded argument-less — and attaching under either reading is wrong under the other.
	// Nothing is attached, and the returned index is the undecided bubble.
	matchAmbiguous
)

// matchBlock finds the bubble for one content block: forward from the cursor first, then a
// bounded look-behind over bubbles the forward scans skipped. It also returns the evidence the
// match was made on, which the caller records per bubble for repeat detection.
func matchBlock(blk block, role string, events []*bubble, cursor int, used []bool, consumedEv []evidence, consumedN []int, declined []bool) (int, matchOutcome, evidence, int) {
	wantType := 2
	if role == "user" {
		wantType = 1
	}

	// weigh is one candidate's evidence, shared by both scan directions. The second value is
	// argsEvidence's strength; text evidence has no gradation, so its positives carry 1.
	weigh := func(b *bubble) (evidence, int) {
		if b.Type != 0 && b.Type != wantType {
			return evidenceNegative, 0
		}
		switch blk.Type {
		case "tool_use":
			if b.ToolFormerData == nil {
				return evidenceNegative, 0
			}
			// Arguments, not names: the transcript's display name and the store's
			// internal name differ, so the arguments are what tells two calls of the
			// same tool apart.
			return argsEvidence(blk.Input, b.ToolFormerData)

		case "text":
			if b.ToolFormerData != nil {
				return evidenceNegative, 0
			}
			// A reasoning bubble aligns only on real overlap: the lenient empty-side
			// rule below would let an empty text block consume it.
			if rt := b.reasoningText(); rt != "" {
				if strictOverlap(string(blk.Text), rt) {
					return evidencePositive, 1
				}
				return evidenceNegative, 0
			}
			if strictOverlap(string(blk.Text), b.Text) {
				return evidencePositive, 1
			}
			if textOverlap(string(blk.Text), b.Text) {
				// The lenient rule confirms nothing: positional evidence only.
				return evidenceNeutral, 0
			}
			return evidenceNegative, 0

		default:
			// An unknown block type means Cursor changed; the loud outcome is right.
			return evidenceNegative, 0
		}
	}

	// A positive is not returned on sight: two bubbles can both agree in full, and the one
	// agreeing on MORE of the call is the call — strength ranks positives, position tie-breaks.
	// Lesser grades are collected here and ranked only after both scans, because a partial
	// identifies the call only when it is discriminative: see the ranking below. Name
	// compatibility gates only the grades where position is the whole claim.
	bestPos, bestPosN := -1, 0
	partials := 0
	fwdPartial, fwdNeutral, fwdWeak := -1, -1, -1
	for i := cursor; i < len(events) && i < cursor+lookAhead; i++ {
		// Nothing at or past the cursor is consumed — consuming a bubble always
		// advances the cursor past it — so this scan needs no used check.
		if declined[i] {
			continue
		}
		ev, n := weigh(events[i])
		switch ev {
		case evidencePositive:
			if n > bestPosN {
				bestPos, bestPosN = i, n
			}
		case evidencePartial:
			partials++
			if fwdPartial < 0 {
				fwdPartial = i
			}
		case evidenceNeutral:
			if fwdNeutral < 0 && (blk.Type != "tool_use" || namesCompatible(string(blk.Name), events[i].ToolFormerData)) {
				fwdNeutral = i
			}
		case evidenceWeak:
			if fwdWeak < 0 && namesCompatible(string(blk.Name), events[i].ToolFormerData) {
				fwdWeak = i
			}
		}
	}
	if bestPos >= 0 {
		return bestPos, matchFound, evidencePositive, bestPosN
	}

	// Look-behind, over what the forward scans stepped over: within one turn the store's header
	// order and the transcript's block order can disagree, so a call's own bubble can end up
	// behind the cursor. A bubble whose arguments agree in full is the same call wherever the
	// header order put it; on any lesser grade a forward candidate wins, since a bubble behind
	// the cursor has been passed over once already.
	repeat := false
	repeatEv, repeatN := evidenceNegative, 0
	bhdPartial, bhdNeutral, bhdWeak := -1, -1, -1
	for i := cursor - 1; i >= 0; i-- {
		if used[i] {
			// A consumed bubble still testifies, but only to a block relating to it
			// exactly as its consumer did: a claimant agreeing better is the bubble's
			// real owner arriving after a positional fallback took it, and one agreeing
			// worse is a different call sharing values with it. Unbounded, unlike the
			// windows — nothing is consumed here.
			if blk.Type == "tool_use" && consumedEv[i] >= evidencePartial {
				if ev, n := weigh(events[i]); ev == consumedEv[i] && n == consumedN[i] {
					repeat = true
					if ev > repeatEv || (ev == repeatEv && n > repeatN) {
						repeatEv, repeatN = ev, n
					}
				}
			}
			continue
		}
		if i < cursor-lookBehind || declined[i] {
			continue
		}
		ev, n := weigh(events[i])
		if blk.Type != "tool_use" && ev != evidencePositive {
			// No positional look-behind for prose: text that did not overlap is a far
			// weaker claim than an argument-less tool call at a known position.
			continue
		}
		switch ev {
		case evidencePositive:
			if n > bestPosN {
				bestPos, bestPosN = i, n
			}
		case evidencePartial:
			partials++
			if bhdPartial < 0 {
				bhdPartial = i
			}
		case evidenceNeutral:
			if bhdNeutral < 0 && namesCompatible(string(blk.Name), events[i].ToolFormerData) {
				bhdNeutral = i
			}
		case evidenceWeak:
			if bhdWeak < 0 && namesCompatible(string(blk.Name), events[i].ToolFormerData) {
				bhdWeak = i
			}
		}
	}
	if bestPos >= 0 {
		return bestPos, matchFound, evidencePositive, bestPosN
	}

	// A lone partial wins outright, either window: it is the only bubble carrying that value.
	// Plural partials cannot discriminate — every sibling grep in a turn agrees through the
	// shared workspace path — so they join the positional pool with the neutrals, where forward
	// beats look-behind and first-in-window decides. Weak candidates come last: their values
	// half-belong to some other call.
	best := -1
	if partials == 1 {
		best = fwdPartial
		if best < 0 {
			best = bhdPartial
		}
	} else {
		fwdTier := fwdNeutral
		if fwdPartial >= 0 && (fwdTier < 0 || fwdPartial < fwdTier) {
			fwdTier = fwdPartial
		}
		bhdTier := max(bhdNeutral, bhdPartial)
		switch {
		case fwdTier >= 0:
			best = fwdTier
		case bhdTier >= 0:
			best = bhdTier
		case fwdWeak >= 0:
			best = fwdWeak
		default:
			best = bhdWeak
		}
	}

	if repeat {
		// Three shapes contradict the dedup reading. An unconsumed bubble agreeing in full
		// anywhere in the store proves the run WAS recorded, just outside the windows, so the
		// loud outcome stands. A candidate explaining the block at least as well as the
		// consumed bubble leaves the dedup reading nothing to offer — edit_file_v2 records
		// only the path, so a turn of edits to one file is all mutual repeats otherwise. And a
		// candidate recording nothing at all may be the second run itself, undecidably.
		for i := range events {
			if used[i] || declined[i] {
				continue
			}
			if ev, _ := weigh(events[i]); ev == evidencePositive {
				return -1, matchNone, evidenceNegative, 0
			}
		}
		if best >= 0 {
			ev, n := weigh(events[best])
			if ev == evidenceNeutral {
				return best, matchAmbiguous, evidenceNeutral, 0
			}
			if ev > repeatEv || (ev == repeatEv && n >= repeatN) {
				return best, matchFound, ev, n
			}
		}
		// The repeat outranks what remains: preferring a worse-agreeing candidate would
		// attach a different call's result and consume the bubble that call needs.
		return -1, matchRepeat, evidenceNegative, 0
	}
	if best >= 0 {
		ev, n := weigh(events[best])
		return best, matchFound, ev, n
	}
	return -1, matchNone, evidenceNegative, 0
}

// strictOverlap reports whether two texts genuinely share their prose. Stricter than
// textOverlap on purpose: an empty side is a non-match here, never a free pass.
func strictOverlap(transcript, stored string) bool { return overlap(transcript, stored, false) }

// textOverlap reports whether a transcript text block and a bubble carry the same prose. The
// transcript wraps user text in tags the store does not, so a normalised core is compared.
// An empty side cannot contradict: position in the ordered list stands.
func textOverlap(transcript, stored string) bool { return overlap(transcript, stored, true) }

func overlap(transcript, stored string, emptyMatches bool) bool {
	t := normaliseText(transcript)
	s := normaliseText(stored)
	if t == "" || s == "" {
		return emptyMatches
	}
	if strings.Contains(t, s) || strings.Contains(s, t) {
		return true
	}
	// Compare a prefix: the store truncates long prose in some generations.
	n := min(len(t), len(s), 64)
	return t[:n] == s[:n]
}

func normaliseText(s string) string {
	// Strip the transcript's wrapper tags, which the store side does not have.
	for _, tag := range []string{"timestamp", "user_query"} {
		s = stripTag(s, tag)
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(s)
}

func stripTag(s, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], close)
		if j < 0 {
			return s[:i]
		}
		inner := s[i+len(open) : i+j]
		if tag == "user_query" {
			// The query IS the prose. Keep the contents, drop the tags.
			s = s[:i] + inner + s[i+j+len(close):]
		} else {
			s = s[:i] + s[i+j+len(close):]
		}
	}
}

// isRedactedReasoning matches the literal placeholder Cursor writes instead of reasoning.
func isRedactedReasoning(text string) bool {
	return strings.TrimSpace(text) == "[REDACTED]"
}

func fromBubble(b *bubble) *blockEnrich {
	model := b.ModelName
	if model == "" && b.ModelInfo != nil {
		model = b.ModelInfo.ModelName
	}
	e := &blockEnrich{
		BubbleID:       b.BubbleID,
		CreatedAt:      b.CreatedAt,
		RequestID:      b.RequestID,
		CheckpointID:   b.CheckpointID,
		TurnDurationMs: b.TurnDurationMs,
		ModelName:      model,
	}
	if t := b.ToolFormerData; t != nil {
		e.ToolCallID = t.ToolCallID
		e.ToolName = cmp.Or(t.Name, string(t.Tool))
		e.Status = t.Status
		e.Result = t.Result
	}
	return e
}

// writeLine emits one output record. No HTML escaping: the output hash is the change signal, and
// json.Encoder's default < > & escaping would make it depend on the transcript's punctuation.
func writeLine(w *bytes.Buffer, l outLine) error {
	body, err := marshalCompact(l)
	if err != nil {
		return err
	}
	w.Write(body)
	w.WriteByte('\n')
	return nil
}

func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline; writeLine adds its own, so trim this one.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
