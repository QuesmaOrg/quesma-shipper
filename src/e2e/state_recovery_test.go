// The fingerprint document is a cache of what the archive holds, not the authority on it: a
// document that cannot be loaded is replaced, and the plane answers for every object the empty
// store does not know, so the loss costs a re-hash and a HEAD per object, not a re-upload.
package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

const grownLine = `{"type":"user","uuid":"u3","message":{"role":"user","content":[{"type":"text","text":"one more"}]}}`

func TestALostDocumentReShipsOnlyWhatChanged(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaudeSession(t, w, username, claudeSessionID)
	grown := stageClaudeSession(t, w, username, numericSessionID)
	writeConfig(t, w, "sources:\n  - id: project-map\n    enabled: false\n  - id: claude-account\n    enabled: false\n")

	runOneShot(t)
	if got := len(mirrorObjects(collect(t, w))); got != 2 {
		t.Fatalf("the first run stored %d transcripts, want the two staged", got)
	}

	appendLine(t, grown, grownLine)
	if err := os.WriteFile(filepath.Join(statePath(w), engine.FileName), []byte("this is not a document\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runOneShot(t)
	w.plane.assertClean(t)

	// Both commit: the plane answered for the unchanged one and the grown one was PUT, so
	// the unchanged key keeps its single version.
	if got := countOf(shippedFromLog(t, w), claudeSource); got != 2 {
		t.Errorf("the run over the lost document shipped %d transcripts, want both", got)
	}
	present := w.plane.answeredPresent()
	for _, o := range mirrorObjects(collect(t, w)) {
		changed := strings.Contains(string(o.Payload), "one more")
		want := 1
		if changed {
			want = 2
		}
		if got := w.store.versions(o.Key); got != want {
			t.Errorf("%s has %d versions, want %d", o.Key, got, want)
		}
		if slices.Contains(present, o.Key) == changed {
			t.Errorf("%s answered present=%v, changed=%v", o.Key, !changed, changed)
		}
	}

	// The replacement document loads, and the next run trusts it: nothing to send.
	if _, err := engine.Peek(statePath(w)); err != nil {
		t.Fatalf("the replacement document does not load: %v", err)
	}
	out := runOneShot(t)
	if got := countOf(shippedFromLog(t, w), claudeSource); got != 0 {
		t.Errorf("the run after recovery shipped %d transcripts again:\n%s", got, out)
	}
	if counts := summary(t, out); counts["unchanged"] == 0 {
		t.Errorf("the run after recovery reported nothing unchanged:\n%s", out)
	}
}
