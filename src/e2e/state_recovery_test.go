// The fingerprint document is a cache of what the archive holds, not the authority on it: a
// lost or reset document costs a re-hash and a HEAD per object, and the plane answers for every
// object it holds under the same raw and scrubbed bytes, so only the rest is uploaded again.
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
	writeConfig(t, w, "sources:\n  - id: claude-account\n    enabled: false\n")

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

// No recognisable shape, and spaced so no token is long enough for the entropy rule: only a
// configured name catches it.
const (
	houseSecret = "plainhousevalue"
	houseLine   = `{"type":"user","uuid":"u3","message":{"role":"user","content":[{"type":"text","text":"HOUSE_SIG: ` + houseSecret + `"}]}}`
)

// The raw bytes never change here, only the scrub rules: a reset must replace the object the new
// rule rewrites, and still be answered for the one it leaves alone.
func TestAResetAfterAScrubChangeReplacesOnlyWhatTheRuleRewrites(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaudeSession(t, w, username, claudeSessionID)
	appendLine(t, stageClaudeSession(t, w, username, numericSessionID), houseLine)
	const base = "sources:\n  - id: claude-account\n    enabled: false\n"
	writeConfig(t, w, base)

	runOneShot(t)
	var rewritten, untouched string
	for _, o := range mirrorObjects(collect(t, w)) {
		if strings.Contains(string(o.Payload), houseSecret) {
			rewritten = o.Key
		} else {
			untouched = o.Key
		}
	}
	if rewritten == "" || untouched == "" {
		t.Fatalf("the first run did not store one transcript with the unscrubbed value and one without")
	}

	// With the fingerprints in place the new rule reaches nothing already shipped.
	writeConfig(t, w, base+"scrub:\n  secret_key_names:\n    - HOUSE_SIG\n")
	runOneShot(t)
	if got := countOf(shippedFromLog(t, w), claudeSource); got != 0 {
		t.Fatalf("a scrub change alone re-shipped %d transcripts", got)
	}

	run(t, "state", "reset", "--apply")
	runOneShot(t)
	w.plane.assertClean(t)
	present := w.plane.answeredPresent()
	if slices.Contains(present, rewritten) || !slices.Contains(present, untouched) {
		t.Fatalf("answered present for %v, want only the transcript the rule leaves alone", present)
	}
	if got := w.store.versions(rewritten); got != 2 {
		t.Errorf("the rewritten transcript has %d versions, want 2", got)
	}
	if got := w.store.versions(untouched); got != 1 {
		t.Errorf("the untouched transcript has %d versions, want 1", got)
	}
	for _, o := range mirrorObjects(collect(t, w)) {
		if strings.Contains(string(o.Payload), houseSecret) {
			t.Errorf("%s still holds the value the new rule redacts", o.Key)
		}
	}

	// Under the rules the archive was just written with, a second reset sends nothing.
	run(t, "state", "reset", "--apply")
	runOneShot(t)
	again := w.plane.answeredPresent()[len(present):]
	if !slices.Contains(again, rewritten) || !slices.Contains(again, untouched) {
		t.Errorf("the second reset was answered present for %v, want both transcripts", again)
	}
	if w.store.versions(rewritten) != 2 || w.store.versions(untouched) != 1 {
		t.Errorf("the second reset wrote again: %d and %d versions", w.store.versions(rewritten), w.store.versions(untouched))
	}
}
