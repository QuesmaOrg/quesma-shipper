package engine_test

import (
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
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
	// The archive answers already_present for bytes it holds, as the control plane's probe does.
	f.port.verdict = func(_, _ int, o engine.PreparedObject) error {
		if obj, ok := f.port.get(o.Key); ok && obj.Metadata["source-hash"] == o.SourceHash {
			return engine.ErrAlreadyPresent
		}
		return nil
	}
	f.writeTranscript("public/before.jsonl", line1)
	apply("sources: [{id: claude-code-transcripts, enabled: true}]")
	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("before the pause: %+v", rep)
	}
	before := f.port.keys()[0]
	apply("sources: [{id: claude-code-transcripts, enabled: false}]")
	f.writeTranscript("public/during.jsonl", line2)
	f.writeTranscript("private/secret.jsonl", privateLine)
	if rep := f.run(); rep.Shipped != 0 {
		t.Fatalf("paused source shipped: %+v", rep)
	}
	apply("sources: [{id: claude-code-transcripts, exclude_add: ['projects/private/**']}]")
	f.port.reset()
	// The new exclusions reset the source's local record, so the file shipped before the pause is read again.
	if rep := f.run(); rep.Shipped != 2 || f.port.putCount() != 1 {
		t.Fatalf("resume should re-read both public files and upload only the new one: %+v, %d uploads", rep, f.port.putCount())
	}
	if obj, _ := f.port.get(before); obj.Versions != 1 {
		t.Fatalf("file shipped before the pause was uploaded again: %d versions", obj.Versions)
	}
	if keys := f.port.keys(); len(keys) != 2 {
		t.Fatalf("excluded file shipped: %v", keys)
	}
	if rep := f.run(); rep.Shipped != 0 {
		t.Fatalf("resume did not retain upload progress: %+v", rep)
	}
}

// Distinct from line1, so a dedup on content cannot hide that the excluded file was read.
const privateLine = `{"type":"user","uuid":"u9","sessionId":"s9","message":{"content":[{"type":"text","text":"private"}]}}` + "\n"
