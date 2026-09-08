package engine_test

import (
	"context"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

// Every run closes its clock and totals its bytes; the defer runs after the store flush, because
// the run is not over while state is still being written.
func TestARunReportsItsFinishTimeAndItsBytes(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)
	f.writeTranscript("p/b.jsonl", line1+line2)

	rep := f.run()

	if rep.FinishedAt.IsZero() {
		t.Fatal("the run left no finish time; every rate the CLI prints divides by it")
	}
	if rep.FinishedAt.Before(rep.StartedAt) {
		t.Errorf("finished %s before it started %s", rep.FinishedAt, rep.StartedAt)
	}
	if rep.BytesRead == 0 {
		t.Error("two transcripts were read and BytesRead is zero")
	}
	if rep.BytesSealed == 0 {
		t.Error("two transcripts shipped and BytesSealed is zero")
	}
	if rep.MedianFileBytes == 0 {
		t.Error("two files shipped and there is no median")
	}
	if rep.BytesRead < rep.MedianFileBytes {
		t.Errorf("the median file (%d B) is larger than everything read (%d B)",
			rep.MedianFileBytes, rep.BytesRead)
	}
}

// A run that stopped early still says how long it ran: a zero clock drops the CLI's line.
func TestACancelledRunStillCarriesItsClock(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := engine.Run(ctx, f.store, f.opts())
	if err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if rep.FinishedAt.IsZero() {
		t.Error("a cancelled run left no finish time")
	}
}
