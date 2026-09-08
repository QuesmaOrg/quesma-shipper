package engine

import (
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

func TestSummarizeTotalsWhatTheOutcomesRecord(t *testing.T) {
	rep := Report{Sources: []SourceOutcome{
		{SourceID: "a", Files: []FileOutcome{
			{Decision: formats.DecisionShipped, BytesIn: 1000, BytesOut: 400},
			{Decision: formats.DecisionShipped, BytesIn: 3000, BytesOut: 900},
			// Read and hashed: the bytes came off the machine, so they count even unsent.
			{Decision: formats.DecisionUnchanged, BytesIn: 500},
			// The pre-filter never opened this one, and a file nobody read cost no bytes.
			{Decision: formats.DecisionUnchanged},
		}},
		{SourceID: "b", Files: []FileOutcome{
			{Decision: formats.DecisionFailed, BytesIn: 200},
		}},
	}}
	summarize(&rep)

	if rep.BytesRead != 4700 {
		t.Errorf("BytesRead = %d, want 4700", rep.BytesRead)
	}
	if rep.BytesSealed != 1300 {
		t.Errorf("BytesSealed = %d, want 1300", rep.BytesSealed)
	}
	// Over shipped files only: a failed 200-byte read does not pull the median down.
	if rep.MedianFileBytes != 3000 {
		t.Errorf("MedianFileBytes = %d, want 3000", rep.MedianFileBytes)
	}
}

// An enricher payload is built from a database and carries a BytesIn even when nothing ships, so
// counting it would keep an idle tick from ever totalling zero.
func TestSummarizeLeavesOutDerivedObjects(t *testing.T) {
	rep := Report{Sources: []SourceOutcome{{
		SourceID: "cursor-transcripts",
		Enriched: 1,
		Files: []FileOutcome{
			{Decision: formats.DecisionShipped, RelPath: "sessions/a.jsonl", BytesIn: 1000, BytesOut: 400},
			// Derived outcomes as derived.go builds them, shipped and unchanged.
			{Decision: formats.DecisionShipped, Derived: true, NativePath: "sessions/a.jsonl", BytesIn: 8000, BytesOut: 2000},
			{Decision: formats.DecisionUnchanged, Derived: true, NativePath: "sessions/b.jsonl", BytesIn: 4000},
		},
	}}}
	summarize(&rep)

	if rep.BytesRead != 1000 || rep.BytesSealed != 400 {
		t.Errorf("derived bytes leaked into the totals: read %d sealed %d, want 1000 and 400",
			rep.BytesRead, rep.BytesSealed)
	}
	// The median is over the input size of files the run read; 8000 was never a file.
	if rep.MedianFileBytes != 1000 {
		t.Errorf("MedianFileBytes = %d, want 1000", rep.MedianFileBytes)
	}
}

// The project map is written by the shipper itself and re-ships on every run, so counting it
// would mean an idle machine never totals zero bytes.
func TestSummarizeLeavesOutWhatTheShipperWroteItself(t *testing.T) {
	rep := Report{Sources: []SourceOutcome{
		{SourceID: "project-map", Emitted: true, Files: []FileOutcome{
			{Decision: formats.DecisionShipped, BytesIn: 203, BytesOut: 1100},
		}},
	}}
	summarize(&rep)

	if rep.BytesRead != 0 || rep.BytesSealed != 0 || rep.MedianFileBytes != 0 {
		t.Errorf("an idle run totalled read %d sealed %d median %d, want zeroes",
			rep.BytesRead, rep.BytesSealed, rep.MedianFileBytes)
	}
}

func TestSummarizeOfAnEmptyRunIsAllZeroes(t *testing.T) {
	rep := Report{}
	summarize(&rep)
	if rep.BytesRead != 0 || rep.BytesSealed != 0 || rep.MedianFileBytes != 0 {
		t.Errorf("an empty run summarised to %+v", rep)
	}
}
