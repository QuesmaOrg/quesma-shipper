package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func TestOversizeReportingSurvivesTheUploadBudget(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", "{\"type\":\"user\"}\n")
	f.writeTranscript("p/b.jsonl", "{\"type\":\"user\"}\n")
	f.writeTranscript("p/big.jsonl", strings.Repeat("x", 128))
	f.writeTranscript("p/excluded.jsonl", strings.Repeat("x", 256))
	f.eff.MaxFilesPerRun = 1
	f.eff.Sources[0].MaxFileBytes = 32
	f.eff.Sources[0].Exclude = []string{"**/excluded.jsonl"}
	other := f.eff.Sources[0]
	other.ID, other.MaxFileBytes = "other-transcripts", 64
	f.eff.Sources = append(f.eff.Sources, other)
	called := false
	f.runWith(func(o *engine.Options) {
		o.Heartbeat = func(_ context.Context, rep engine.Report) error {
			called = true
			if !rep.Truncated {
				t.Error("test did not exhaust the upload budget")
			}
			if len(rep.Sources) != 2 {
				t.Fatalf("sources: %+v", rep.Sources)
			}
			for _, s := range rep.Sources {
				if s.Oversize != 1 || len(s.OversizeFiles) != 1 {
					t.Fatalf("oversize report: %+v", s)
				}
				wantLimit := int64(32)
				if s.SourceID == other.ID {
					wantLimit = 64
				}
				file := s.OversizeFiles[0]
				if file.RelPath != "projects/p/big.jsonl" || file.Size != 128 || file.Limit != wantLimit {
					t.Fatalf("wrong per-source metadata: %+v", file)
				}
				if s.Health != sources.Collected {
					t.Fatalf("oversize file prevented healthy collection: %s", s.Health)
				}
			}
			return nil
		}
	})
	if !called {
		t.Fatal("no heartbeat")
	}
}
