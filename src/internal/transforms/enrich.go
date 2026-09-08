// Package enrich is the enricher contract and registry: a compiled per-source hook running
// after a flush's raw units are staged and BEFORE redaction, joining a store of higher fidelity
// (Cursor's SQLite holds the tool results, tool-call ids and timestamps its JSONL lacks). It is
// the ONLY capture path for those fields, since no rows ship; raw files ship regardless, so it
// fails open, and its output is deterministic and versioned so the output hash is a change signal.
package transforms

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Status is the outcome of one enrichment. Anything but StatusOK means that window's DB-side
// fields are lost until a release fixes the join, so these are health alarms.
type Status string

const (
	// StatusOK means the derived object is complete.
	StatusOK Status = "ok"

	// StatusSkipped means there was legitimately nothing to derive. Not an alarm.
	StatusSkipped Status = "skipped"

	// StatusMismatch means the join did not line up. THE alarm: the alignment rules are
	// undocumented vendor behaviour, and a drifted join silently loses the fields it carries.
	StatusMismatch Status = "mismatch"

	// StatusError means the read or the computation failed.
	StatusError Status = "error"
)

// RawUnit is one staged raw file an enricher may read: staged CONTENT, not a path to re-open,
// so derived_from attests to the bytes that shipped and enrichers get no filesystem access.
type RawUnit struct {
	// NativePath is the file's path, for naming the derived object and for the join key.
	NativePath string

	// Content is the raw pre-redaction bytes, as staged.
	Content []byte

	// SourceHash becomes an entry in derived_from.
	SourceHash string
}

// Input is what an enricher gets.
type Input struct {
	SourceID string

	// Units are the staged raw units of this source in this flush.
	Units []RawUnit

	// DBPath is the declared agent database; empty means absent, which is not an error.
	DBPath string

	// ScratchDir is where the read ladder may put a snapshot: never beside the source.
	ScratchDir string
}

// Derived is one derived object.
type Derived struct {
	// The derived object's own path, which becomes its mirror key. By convention
	// <input path>.enriched.jsonl, so it sits beside its input in any listing.
	NativePath string

	// Payload is the derived bytes, pre-redaction, taking the same path as a raw file.
	Payload []byte

	// DerivedFrom is the source hash of every raw input this was computed from.
	DerivedFrom []string

	// OutputHash is the change signal: the object re-ships only when it changes, which is
	// what determinism buys.
	OutputHash string

	Status Status

	// Counts events that did not align. Non-zero with StatusOK is impossible by contract:
	// a mismatch aborts the derived entry.
	Mismatches int

	// Store shortfalls the join explains, shipped native-only inside a StatusOK object.
	// One counter per class, so a conversation with holes is not byte-identical downstream
	// to a complete one.
	Repeats int
	Tail    int

	// Events the evidence could not decide, where the join attached nothing rather than
	// guess. An undecided hole, not an explained one.
	Ambiguous int

	// Transcript lines before the tail that did not decode. An alarm, unlike the three
	// above: those lines' blocks never reached the join, so their enrichment is lost while
	// the object still ships.
	LineDecodeErrors int

	// Read provenance: the rows never ship, so this is the only account of their origin.
	DBReadMethod string
	DBKeyspaces  []string
	DBRowsRead   int
}

// EnrichResult is one enricher's whole contribution to a flush.
type EnrichResult struct {
	EnricherID string
	Version    int

	// Objects are the derived objects to ship. Empty is a normal outcome.
	Objects []Derived

	// Inputs that produced nothing, split because only Mismatched is an alarm.
	Skipped    int
	Mismatched int
	Errors     int

	// Human-readable reasons for the audit log and `doctor`. Never payload bytes or redacted
	// values: diagnostics must not become a side channel for the content being read.
	// Alarms only — each note explains data that did not ship.
	Notes []string

	// Informational notes about objects that DID ship. Kept apart from Notes so no reporting
	// path has to re-parse note text to decide whether it is looking at loss.
	Infos []string
}

// Enricher is a compiled per-source hook.
type Enricher interface {
	// Identify the code that produced an output, so derived objects are supersedable.
	ID() string
	Version() int

	// The DECLARED read scope, fixed at registration: a hook that could widen its own scope
	// at runtime would make the compiled ceiling meaningless.
	Table() string
	Keyspaces() []string

	// Per-platform locations of the agent database in preference order, same ~ and $VAR
	// syntax as catalog roots. Compiled in, never configured: an arbitrary SQLite path is
	// one the catalog never approved. nil when the enricher has no database.
	DBCandidates() []string

	// Whether the enricher derives from staged raw units. A unit-free enricher runs on EVERY
	// flush, because its input is the agent's store, which moves on its own schedule:
	// otherwise an idle but logged-in install never reports its account at all.
	NeedsUnits() bool

	// Enrich derives objects, returning a result rather than an error for anything short of a
	// programming fault: no enricher failure should stop a flush.
	Enrich(Input) EnrichResult
}

// Registry is the compiled set. Config enables or disables entries but can never add one,
// which would mean config installing transformation code.
type Registry struct {
	byID map[string]Enricher
}

// NewRegistry builds the compiled registry.
func NewRegistry(es ...Enricher) *Registry {
	r := &Registry{byID: map[string]Enricher{}}
	for _, e := range es {
		r.byID[e.ID()] = e
	}
	return r
}

// For returns a registered enricher.
func (r *Registry) For(id string) (Enricher, error) {
	e, ok := r.byID[id]
	if !ok {
		// Refused, never ignored: an unknown enricher would otherwise silently collect
		// raw-only and lose the DB-side fields with no signal.
		return nil, fmt.Errorf("enrich: no enricher %q in this build", id)
	}
	return e, nil
}

// Hash is the output-hash helper every enricher uses, so the change signal is computed one way.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
