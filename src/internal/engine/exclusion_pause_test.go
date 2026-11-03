package engine_test

import (
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func TestFilesWrittenDuringExclusionCompatibilityPauseShipAfterUpgrade(t *testing.T) {
	f := newFixture(t)
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	apply := func(raw string) {
		doc, err := config.ParseServedDocument([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		eff, err := config.Resolve(config.Input{Catalog: catalog, Env: sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}, StateDir: f.stateDir, Layers: []config.LayeredDocument{{Layer: config.LayerRemote, Doc: doc}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range eff.Sources {
			if source.ID == "claude-code-transcripts" {
				f.eff.Sources = []sources.Resolved{source}
				return
			}
		}
		t.Fatal("missing source")
	}
	f.writeTranscript("public/before.jsonl", line1)
	apply("sources: [{id: claude-code-transcripts, enabled: false}]")
	if rep := f.run(); rep.Shipped != 0 {
		t.Fatalf("paused source shipped: %+v", rep)
	}
	f.writeTranscript("public/during.jsonl", line2)
	f.writeTranscript("private/secret.jsonl", line1)
	if rep := f.run(); rep.Shipped != 0 {
		t.Fatalf("paused source shipped new file: %+v", rep)
	}
	apply("sources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]")
	if rep := f.run(); rep.Shipped != 2 {
		t.Fatalf("resume should ship both retained public files: %+v", rep)
	}
	if rep := f.run(); rep.Shipped != 0 {
		t.Fatalf("resume did not retain upload progress: %+v", rep)
	}
}
