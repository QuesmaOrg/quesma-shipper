package e2e

import (
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	state "github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

// Written once: a typo in a source id turns an assertion into one that can never fail.
const (
	claudeSource = "claude-code-transcripts"
	cursorSource = "cursor-transcripts"
)

// Invariants, not goldens: each states a property that must hold for any input, so it keeps
// meaning the same thing as the fixtures grow. Goldens live next door.

func TestNoShippedByteCarriesASeededSecret(t *testing.T) {
	// The one failure here that is a disaster rather than a bug, so it goes first.
	w := stageWorld(t)
	username := realUsername(t)
	stageClaude(t, w, username)
	runOneShot(t)

	for _, o := range collect(t, w) {
		for _, secret := range []string{seededGitHubToken, seededAWSKey, seededShapelessSecret} {
			if strings.Contains(string(o.Payload), secret) {
				t.Errorf("%s: a seeded secret survived redaction: %s", o.Key, secret)
			}
		}
	}
}

// The counts are the product: a redaction rule widened to catch "tokens" would take every usage
// figure with it while the run looked healthy.
func TestTokenCountsSurviveRedaction(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)

	var seen bool
	for _, o := range bySourceID(mirrorObjects(collect(t, w)), claudeSource) {
		for _, want := range []string{`"input_tokens":120`, `"output_tokens":340`, `"cache_read_input_tokens":9000`} {
			if strings.Contains(string(o.Payload), want) {
				seen = true
			} else if strings.Contains(string(o.Payload), "input_tokens") {
				t.Errorf("%s: usage numbers were altered; wanted %s", o.Key, want)
			}
		}
	}
	if !seen {
		t.Fatal("no usage numbers in any shipped payload; this test would pass vacuously")
	}
}

func TestNoShippedByteCarriesTheOSUsername(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaude(t, w, username)
	runOneShot(t)

	objects := mirrorObjects(collect(t, w))
	if len(objects) == 0 {
		t.Fatal("nothing was collected; the rest of this test would pass vacuously")
	}
	for _, o := range objects {
		// Both shapes the stores use, and from the manifest's native_path as well as the payload.
		if strings.Contains(string(o.Payload), username) {
			t.Errorf("%s: the OS username survived in the payload", o.Key)
		}
		if strings.Contains(o.Manifest.NativePath, username) {
			t.Errorf("%s: the OS username survived in native_path: %s", o.Key, o.Manifest.NativePath)
		}
		// Only where the fixture planted one: the sidecar's native path is the state directory,
		// which sits under home on a real machine but not here.
		if isTranscript(o) && !strings.Contains(o.Manifest.NativePath, "__USER__") {
			t.Errorf("%s: native_path carries no placeholder: %s", o.Key, o.Manifest.NativePath)
		}
	}
}

// Objects whose paths came from an agent's own store, the only place a username can appear.
func isTranscript(o object) bool {
	switch o.Manifest.SourceID {
	case "claude-code-transcripts", "cursor-transcripts", "codex-rollouts":
		return true
	}
	return false
}

func TestKeysRevealNothingAboutTheFileTheyName(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaude(t, w, username)
	stageCursor(t, w, username, cursorConversation2026_07(), true)
	runOneShot(t)

	for _, o := range mirrorObjects(collect(t, w)) {
		// The source id is deliberately in the clear so a reader can select by source without a
		// key; everything after it must be opaque.
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		hexPart := strings.TrimSuffix(name, ".age")
		if name == hexPart {
			t.Errorf("%s: does not end in .age", o.Key)
		}
		if _, err := hex.DecodeString(hexPart); err != nil {
			t.Errorf("%s: name is not hex: %v", o.Key, err)
		}
		for _, leak := range []string{username, "demo", ".jsonl", "projects", cursorConv} {
			if strings.Contains(hexPart, leak) {
				t.Errorf("%s: key leaks %q", o.Key, leak)
			}
		}
		if !strings.HasPrefix(o.Key, w.KeyRoot+"/") {
			t.Errorf("%s: outside this install's subtree %s", o.Key, w.KeyRoot)
		}
	}
}

func TestEveryObjectCarriesBothHashesAndItsOwnPayload(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)

	for _, o := range mirrorObjects(collect(t, w)) {
		if o.Manifest.SourceHash == "" {
			t.Errorf("%s: no source_hash — change detection has nothing to compare", o.Key)
		}
		// An empty shipped_hash once shipped in every object, unnoticed because Seal took the
		// manifest by value.
		if o.Manifest.ShippedHash == "" {
			t.Errorf("%s: no shipped_hash — nothing can verify this object", o.Key)
		}
		if o.Manifest.SourceHash == o.Manifest.ShippedHash && o.Manifest.Redaction != nil &&
			o.Manifest.Redaction.Density > 0 {
			t.Errorf("%s: redaction changed bytes but both hashes are equal", o.Key)
		}
		if got := int64(len(o.Payload)); got != o.Manifest.PayloadSize {
			t.Errorf("%s: payload is %d bytes, manifest says %d", o.Key, got, o.Manifest.PayloadSize)
		}
	}
}

func TestSealedAtIsAReadableTimeFromThisRun(t *testing.T) {
	// Not pinned, since app takes it from time.Now(), but a field nothing checks is one that can
	// quietly become empty.
	before := time.Now().UTC().Add(-time.Second)
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)
	after := time.Now().UTC().Add(time.Second)

	for _, o := range collect(t, w) {
		got, err := time.Parse(time.RFC3339, o.Manifest.SealedAt)
		if err != nil {
			t.Errorf("%s: sealed_at %q does not parse: %v", o.Key, o.Manifest.SealedAt, err)
			continue
		}
		if got.Before(before) || got.After(after) {
			t.Errorf("%s: sealed_at %s is outside this run (%s..%s)", o.Key, got, before, after)
		}
	}
}

func TestASecondRunShipsNothing(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	firstOut := runOneShot(t)
	if countOf(shippedFromLog(t, w), claudeSource) != 1 {
		t.Fatalf("the first run did not ship the transcript; the rest would pass vacuously:\n%s", firstOut)
	}

	secondOut := runOneShot(t)
	if got := countOf(shippedFromLog(t, w), claudeSource); got != 0 {
		t.Errorf("a second run over unchanged input shipped the transcript again:\n%s", secondOut)
	}
	if summary(t, secondOut)["unchanged"] == 0 {
		t.Errorf("nothing was reported unchanged:\n%s", secondOut)
	}
}

func TestTouchingEveryFileShipsNothing(t *testing.T) {
	// mtime is a pre-filter and the content hash is the authority: a Cursor session leaves
	// hundreds of files with new mtimes and identical bytes.
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)

	touchEverything(t, w)
	out := runOneShot(t)

	if got := countOf(shippedFromLog(t, w), claudeSource); got != 0 {
		t.Errorf("a new mtime and identical bytes shipped the transcript again:\n%s", out)
	}
}

func TestAppendingOneLineShipsTheFileAgain(t *testing.T) {
	w := stageWorld(t)
	path := stageClaude(t, w, realUsername(t))
	runOneShot(t)
	first := mirrorObjects(collect(t, w))

	appendLine(t, path, `{"type":"user","uuid":"u3","message":{"role":"user","content":[{"type":"text","text":"one more"}]}}`)
	runOneShot(t)
	if got := countOf(shippedFromLog(t, w), claudeSource); got != 1 {
		t.Errorf("one transcript grew and %d were shipped", got)
	}
	second := mirrorObjects(collect(t, w))

	// A grown file keeps its key, an HMAC over the path, so growth shows up as a second version
	// rather than a second object.
	if len(second) != len(first) {
		t.Fatalf("append produced %d objects, want the same %d under new content",
			len(second), len(first))
	}
	if got := w.store.versions(second[0].Key); got != 2 {
		t.Errorf("%s has %d versions after one append, want the original and the grown one",
			second[0].Key, got)
	}
	if second[0].Manifest.SourceHash == first[0].Manifest.SourceHash {
		t.Error("the transcript grew and source_hash did not move")
	}
	if !strings.Contains(string(second[0].Payload), "one more") {
		t.Error("the appended line is not in the shipped payload")
	}
}

func TestCursorPairYieldsADerivedObjectAndKeepsTheRaw(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageCursor(t, w, username, cursorConversation2026_07(), true)
	runOneShot(t)

	objects := bySourceID(mirrorObjects(collect(t, w)), cursorSource)
	if len(objects) != 2 {
		t.Fatalf("want two objects — the raw transcript and the derived join — got %d", len(objects))
	}

	var raw, derived *object
	for i := range objects {
		if objects[i].Manifest.Derived {
			derived = &objects[i]
		} else {
			raw = &objects[i]
		}
	}
	if raw == nil || derived == nil {
		t.Fatal("want exactly one raw object and one derived object")
	}
	if derived.Manifest.EnrichStatus != "ok" {
		t.Errorf("derived object reports enrich_status %q, want ok", derived.Manifest.EnrichStatus)
	}
	if len(derived.Manifest.DerivedFrom) == 0 {
		t.Error("the derived object does not name what it came from")
	}
	// The point of the join: the store holds the tool output and the transcript does not. Passing
	// for the raw object too would mean the enricher no longer earns its cost.
	const onlyInTheStore = "main.go"
	if !strings.Contains(string(derived.Payload), onlyInTheStore) {
		t.Errorf("the derived payload lacks what only the store has (%q)", onlyInTheStore)
	}
	if strings.Contains(string(raw.Payload), onlyInTheStore) {
		t.Errorf("the raw transcript already had %q — this fixture no longer tests the join", onlyInTheStore)
	}
}

func TestATranscriptTheStoreDoesNotKnowShipsRawAndSaysSo(t *testing.T) {
	// The drift case: when the join stops aligning, the raw transcript must still ship and the
	// manifest must say the join failed. Silence would be data loss that looks like success.
	w := stageWorld(t)
	username := realUsername(t)
	stageCursor(t, w, username, cursorConversation2026_07(), false)
	runOneShot(t)

	objects := bySourceID(mirrorObjects(collect(t, w)), cursorSource)
	if len(objects) != 1 {
		t.Fatalf("want the raw transcript alone, got %d objects", len(objects))
	}
	if objects[0].Manifest.Derived {
		t.Error("a derived object was produced from a store that knows nothing about it")
	}
	if len(objects[0].Payload) == 0 {
		t.Error("the raw transcript shipped empty")
	}
}

// The shipper rewrites its own project map every run and reads it straight back, so an otherwise
// idle run still moves bytes: counting those would put the throughput line under every summary.
func TestAnIdleSyncPrintsNoThroughputLine(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))

	if first := runOneShot(t); !strings.Contains(first, "sealed of") {
		t.Fatalf("the backlog run printed no throughput line; the rest would pass vacuously:\n%s",
			first)
	}
	if second := runOneShot(t); strings.Contains(second, "sealed of") {
		t.Errorf("a sync that read nothing of the user's still reported throughput:\n%s", second)
	}
}

// The console is budgeted and the log is not, so the log is only worth falling back to if
// everything is in it.
func TestEveryShippedLineLandsInTheRunLog(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	out := runOneShot(t)

	console := shippedSources(out)
	if len(console) == 0 {
		t.Fatal("the run shipped nothing; the rest of this test would pass vacuously")
	}
	logged := runLogLines(t, w)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if !slices.Contains(logged, line) {
			t.Errorf("a per-file line never reached the run log:\n%s\nlog:\n%s",
				line, strings.Join(logged, "\n"))
		}
	}
}

// One fixed name, truncated as it opens: the file answers "what did the last sync do" and an
// appending log would answer a different question at unbounded length.
func TestTheRunLogIsTruncatedEachSync(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)
	first := runLogLines(t, w)
	if countOf(shippedSources(strings.Join(first, "\n")), claudeSource) != 1 {
		t.Fatalf("the first sync did not log the transcript; the rest would pass vacuously:\n%s",
			strings.Join(first, "\n"))
	}

	// A second sync over unchanged input says nothing, so that line can only be the first run's.
	runOneShot(t)
	second := runLogLines(t, w)
	if got := countOf(shippedSources(strings.Join(second, "\n")), claudeSource); got != 0 {
		t.Errorf("the log kept the previous run's lines:\n%s", strings.Join(second, "\n"))
	}
}

// The log belongs to whichever sync holds the lock. A manual sync during a scheduled one is
// refused rather than interleaved, and must leave the running run's log where the notice said.
func TestASyncRefusedForTheLockDoesNotTouchTheRunLog(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	runOneShot(t)
	before := runLogLines(t, w)

	// Empty install id, so this stands in for another process rather than opening as this install.
	held, err := state.Open(filepath.Join(w.State, "trajectory-shipper"), "")
	if err != nil {
		t.Fatalf("could not stand in for a running sync: %v", err)
	}
	defer held.Close()

	if out, err := runOneShotExpectingFailure(t, "--quiet"); err == nil {
		t.Fatalf("a second sync was not refused for the lock:\n%s", out)
	}

	if after := runLogLines(t, w); !slices.Equal(before, after) {
		t.Errorf("the refused sync rewrote the running sync's log:\nbefore:\n%s\nafter:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

// --quiet is for cron: it drops the console output and keeps the log, the run's only record.
func TestAQuietSyncStillWritesTheRunLog(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))

	out := runOneShot(t, "--quiet")
	if strings.Contains(out, "shipped") {
		t.Errorf("--quiet printed a summary:\n%s", out)
	}
	logged := runLogLines(t, w)
	if len(logged) == 0 {
		t.Fatal("a quiet sync wrote an empty run log")
	}
	var shippedLines int
	for _, line := range logged {
		if strings.Contains(line, "  shipped (") {
			shippedLines++
		}
	}
	if shippedLines == 0 {
		t.Errorf("the quiet run's log records nothing it shipped:\n%s", strings.Join(logged, "\n"))
	}
}

func TestADisabledSourceShipsNothing(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaude(t, w, username)
	stageCursor(t, w, username, cursorConversation2026_07(), true)
	writeConfig(t, w, "sources:\n  - id: cursor-transcripts\n    enabled: false\n")

	runOneShot(t)

	if got := bySourceID(mirrorObjects(collect(t, w)), cursorSource); len(got) != 0 {
		t.Errorf("a disabled source shipped %d objects", len(got))
	}
	if got := bySourceID(mirrorObjects(collect(t, w)), claudeSource); len(got) == 0 {
		t.Error("disabling one source silenced another")
	}
}

func TestThePauseSwitchStopsCollection(t *testing.T) {
	w := stageWorld(t)
	stageClaude(t, w, realUsername(t))
	run(t, "pause", "1h")

	if got := mirrorObjects(collect(t, w)); len(got) != 0 {
		t.Fatalf("nothing has run yet and %d objects exist", len(got))
	}
	runOneShot(t)
	if got := mirrorObjects(collect(t, w)); len(got) != 0 {
		t.Fatalf("paused, and %d objects were still shipped", len(got))
	}

	run(t, "resume")
	runOneShot(t)
	if got := countOf(shippedFromLog(t, w), claudeSource); got == 0 {
		t.Error("resumed, and nothing was collected")
	}
}
