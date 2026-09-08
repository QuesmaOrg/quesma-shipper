// Package cursorjoin is the cursor-transcript-join enricher: Cursor's transcript JSONL records
// intent with no outcomes, the global state.vscdb holds exactly those missing fields, and no
// store rows ship, so this derived object is their only carrier. Every failure path degrades to
// raw JSONL plus a loud alarm, never to a partial object, which would look complete downstream.
// The alignment rules are undocumented vendor behaviour and drift, hence the mismatch alarm.
package cursorjoin

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// The version travels in every derived object's manifest, so output from a join fixed later
// supersedes output from this one. Bump it whenever the join's answer can change.
const (
	id      = "cursor-transcript-join"
	version = 4
)

// The declared read scope: composer records and per-turn bubbles, nothing else in the table.
const (
	table          = "cursorDiskKV"
	composerPrefix = "composerData:"
	bubblePrefix   = "bubbleId:"
)

// Enricher implements transforms.Enricher.
type Enricher struct{}

func New() *Enricher { return &Enricher{} }

func (*Enricher) ID() string   { return id }
func (*Enricher) Version() int { return version }

// The join derives from the transcript lines, so with no staged units there is nothing to do.
func (*Enricher) NeedsUnits() bool { return true }
func (*Enricher) Table() string {
	return table
}

// Prefixes rather than the whole table: state.vscdb also holds checkpoints, diffs and tokens.
func (*Enricher) Keyspaces() []string {
	return []string{composerPrefix, bubblePrefix}
}

// DBCandidates is where Cursor's global state.vscdb lives, per platform. Compiled in rather than
// configurable: a config path would let a config layer point this enricher at an arbitrary SQLite
// file, past a scope ceiling the compiled catalog never approved. The workspace store is absent.
const cursorStateDB = "Cursor/User/globalStorage/state.vscdb"

func (*Enricher) DBCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"~/Library/Application Support/" + cursorStateDB}
	case "linux":
		return []string{
			"~/.config/" + cursorStateDB,
			"~/.config/cursor/User/globalStorage/state.vscdb",
		}
	case "windows":
		return []string{"$APPDATA/" + cursorStateDB}
	default:
		return nil
	}
}

// Enrich derives one object per transcript unit.
func (e *Enricher) Enrich(in transforms.Input) transforms.EnrichResult {
	res := transforms.EnrichResult{EnricherID: id, Version: version}

	if in.DBPath == "" {
		// Not an alarm: nothing to derive from, and the raw transcripts ship as always.
		res.Skipped = len(in.Units)
		if len(in.Units) > 0 {
			res.Notes = append(res.Notes, "no state.vscdb found: raw transcripts only")
		}
		return res
	}

	read, err := sqliteread.Read(sqliteread.Options{
		Path:        in.DBPath,
		ScratchDir:  in.ScratchDir,
		Table:       table,
		KeyPrefixes: e.Keyspaces(),
	})
	if err != nil {
		// Fail open: this costs the flush's DB-side fields, not the raw transcripts.
		res.Errors = len(in.Units)
		res.Notes = append(res.Notes, "state.vscdb unreadable: "+err.Error())
		return res
	}

	if read.Truncated {
		// A partial view, not a failed one. Rows are ordered by key, so what is missing is a
		// suffix of the keyspace, and the units past the cap are counted as mismatches below.
		res.Notes = append(res.Notes, fmt.Sprintf(
			"state.vscdb row cap reached at %d rows: DB-side fields are missing for whatever "+
				"sorts after the last key read", len(read.Rows)))
	}

	store := indexRows(read.Rows)
	if store.decodeErrors > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d store rows in the declared keyspaces did not decode (the store schema is vendor behaviour and drifts)",
			store.decodeErrors))
	}

	for _, u := range in.Units {
		conv := conversationID(u.NativePath)
		if conv == "" {
			res.Skipped++
			continue
		}
		d, note := e.joinOne(u, conv, store, read)
		if note != "" {
			// Routed by outcome: a note on a shipped object would otherwise be reported as
			// lost data.
			if d.Status == transforms.StatusOK {
				res.Infos = append(res.Infos, note)
			} else {
				res.Notes = append(res.Notes, note)
			}
		}
		// The transcript-side twin of the store decode note above, and like it an alarm: those
		// blocks never reached the join, so their enrichment is lost even when the object ships.
		if d.LineDecodeErrors > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: %d mid-file transcript lines did not decode (the line shape is vendor "+
					"behaviour and drifts): their blocks never reached the join, and the "+
					"derived object ships without their enrichment",
				conv, d.LineDecodeErrors))
		}
		switch d.Status {
		case transforms.StatusOK:
			res.Objects = append(res.Objects, d)
		case transforms.StatusMismatch:
			// A partial object is indistinguishable downstream from a complete one.
			res.Mismatched++
		case transforms.StatusError:
			res.Errors++
		default:
			res.Skipped++
		}
	}
	return res
}

// conversationID is the join key: the transcript's basename, which is the same UUID as
// composerData:<composerUUID>.
func conversationID(nativePath string) string {
	base := filepath.Base(nativePath)
	if !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(base, ".jsonl")
}

// joinOne derives a single conversation.
func (e *Enricher) joinOne(u transforms.RawUnit, conv string, ix *indexed, read sqliteread.Result) (transforms.Derived, string) {
	d := transforms.Derived{
		NativePath:   u.NativePath + ".enriched.jsonl",
		DerivedFrom:  []string{u.SourceHash},
		DBReadMethod: string(read.Method),
		DBKeyspaces:  e.Keyspaces(),
		DBRowsRead:   len(read.Rows),
	}

	c := ix.composers[conv]
	bubbles := ix.bubbles[conv]

	// A conversation with no headers, no inline array and no bubble rows is a draft, not a
	// mismatch. Most composerData rows on a real machine are drafts.
	inline := 0
	headers := 0
	if c != nil {
		inline = len(c.Conversation)
		headers = len(c.FullConversationHeadersOnly)
	}
	if max(headers, inline, len(bubbles)) == 0 {
		d.Status = transforms.StatusSkipped
		return d, ""
	}

	ordered := orderBubbles(c, bubbles, ix.order[conv])
	if len(ordered) == 0 {
		d.Status = transforms.StatusSkipped
		return d, ""
	}

	// Dropped before alignment: left in, they would mismatch rows that never had a counterpart.
	events := make([]*bubble, 0, len(ordered))
	for _, b := range ordered {
		if isScaffolding(b) {
			continue
		}
		events = append(events, b)
	}

	a, err := alignAndRender(u.Content, events)
	if err == nil {
		// Set before the status branches: Enrich reports this loss whether or not an object ships.
		d.LineDecodeErrors = a.lineDecodeErrors
	}
	switch {
	case err != nil:
		d.Status = transforms.StatusError
		return d, fmt.Sprintf("%s: %v", conv, err)
	case a.mismatches > 0:
		d.Status = transforms.StatusMismatch
		d.Mismatches = a.mismatches
		return d, fmt.Sprintf("%s: %d transcript events did not align with the store; "+
			"no derived object, raw transcript ships (the join rules are vendor behaviour and drift)",
			conv, a.mismatches)
	}

	d.Payload = a.out
	d.OutputHash = transforms.Hash(a.out)
	d.Status = transforms.StatusOK
	// Onto the object, not only into a note: the note dies with the flush.
	d.Repeats = a.repeats
	d.Tail = a.tail
	d.Ambiguous = a.ambiguous

	// Informational, not alarms: the store accounting for less than the transcript in ways that
	// are observed vendor behaviour, not rule drift.
	var infos []string
	if a.tail > 0 {
		infos = append(infos, fmt.Sprintf("%d transcript events extend past the store's "+
			"last bubble", a.tail))
	}
	if a.repeats > 0 {
		infos = append(infos, fmt.Sprintf("%d transcript events repeat calls whose bubble "+
			"an identical earlier event already consumed", a.repeats))
	}
	if a.ambiguous > 0 {
		infos = append(infos, fmt.Sprintf("%d transcript events the store evidence cannot "+
			"decide between a deduped re-run and a second unrecorded call — nothing attached "+
			"rather than guessed", a.ambiguous))
	}
	if len(infos) > 0 {
		return d, fmt.Sprintf("%s: %s; carried native-only in the derived object",
			conv, strings.Join(infos, "; "))
	}
	return d, ""
}
