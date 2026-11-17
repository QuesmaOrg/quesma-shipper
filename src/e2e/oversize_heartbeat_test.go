package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	seal "github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestOversizeSessionIsReportedInTheArchiveEveryRun(t *testing.T) {
	v := stageWorld(t)
	username := realUsername(t)
	path := stageClaude(t, v, username)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const limit = 4096
	if len(original) >= limit {
		t.Fatal("fixture must start below the cap")
	}
	writeConfig(t, v, "sources:\n  - id: claude-code-transcripts\n    max_file_bytes: 4096\n  - id: claude-account\n    enabled: false\n")
	runOneShot(t)
	before := v.store.mirrorPuts()
	if len(before) == 0 {
		t.Fatal("the original snapshot never shipped")
	}
	if err := os.Truncate(path, limit*2); err != nil {
		t.Fatal(err)
	}

	for tick := 0; tick < 2; tick++ {
		runOneShot(t)
		beats := v.store.heartbeats()
		if len(beats) != tick+2 {
			t.Fatalf("want one heartbeat every run, got %d", len(beats))
		}
		_, payload, err := seal.Open(beats[len(beats)-1].Body, v.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte(username)) {
			t.Fatalf("heartbeat leaked username: %s", payload)
		}
		var hb engine.Heartbeat
		if err := json.Unmarshal(payload, &hb); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, source := range hb.Sources {
			if source.SourceID != "claude-code-transcripts" {
				continue
			}
			found = true
			if source.Oversize != 1 || len(source.OversizeFiles) != 1 {
				t.Fatalf("missing skipped session: %+v", source)
			}
			file := source.OversizeFiles[0]
			rel, err := filepath.Rel(filepath.Join(v.Home, ".claude"), path)
			if err != nil {
				t.Fatal(err)
			}
			if file.RelPath != formats.ApplyUserPlaceholder(filepath.ToSlash(rel), username) || file.Size != limit*2 || file.Limit != limit {
				t.Fatalf("wrong skipped file metadata: %+v", file)
			}
		}
		if !found {
			t.Fatal("missing source")
		}
		after := v.store.mirrorPuts()
		if len(after) != len(before) {
			t.Fatal("oversize file changed the archive snapshot")
		}
	}

	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	runOneShot(t)
	beats := v.store.heartbeats()
	_, payload, err := seal.Open(beats[len(beats)-1].Body, v.Identity)
	if err != nil {
		t.Fatal(err)
	}
	var hb engine.Heartbeat
	if err := json.Unmarshal(payload, &hb); err != nil {
		t.Fatal(err)
	}
	for _, source := range hb.Sources {
		if source.SourceID == "claude-code-transcripts" && (source.Oversize != 0 || source.OversizeFiles == nil || len(source.OversizeFiles) != 0) {
			t.Fatalf("recovered session still reported as oversize: %+v", source)
		}
	}
	v.plane.assertClean(t)
}
