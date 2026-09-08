package engine_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

const installID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const otherSha = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func key(path string) engine.Key {
	return engine.Key{
		SourceID:   "claude-code-transcripts",
		NativePath: path,
	}
}

// fixedMTime keeps the fixture deterministic. The nanoseconds are not decoration: a whole-second
// mtime cannot catch a serializer that truncates, and no real filesystem hands out whole seconds.
var fixedMTime = time.Date(2026, 7, 30, 10, 0, 0, 987654321, time.UTC)

func fingerprint() engine.Fingerprint {
	return engine.Fingerprint{
		SourceSize:  4096,
		SourceMTime: fixedMTime,
		SourceHash:  sha,
	}
}

// commit is the one-entry CommitAll these tests are written against.
func commit(s *engine.Store, k engine.Key, fp engine.Fingerprint) error {
	return s.CommitAll(map[engine.Key]engine.Fingerprint{k: fp})
}

func open(t *testing.T, dir string) *engine.Store {
	t.Helper()
	s, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFirstRunIsEmptyNotAnError(t *testing.T) {
	s := open(t, t.TempDir())
	if s.Len() != 0 {
		t.Errorf("a fresh store should be empty, has %d entries", s.Len())
	}
	if _, ok := s.Get(key("/x/a.jsonl")); ok {
		t.Error("a fresh store should know nothing")
	}
}

func TestCommitThenReload(t *testing.T) {
	dir := t.TempDir()

	s := open(t, dir)
	want := fingerprint()
	if err := commit(s, key("/x/a.jsonl"), want); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, dir)
	got, ok := s2.Get(key("/x/a.jsonl"))
	if !ok {
		t.Fatal("entry did not survive a reload")
	}
	if got.SourceHash != want.SourceHash || got.SourceSize != want.SourceSize {
		t.Errorf("fingerprint changed across reload:\n got %+v\nwant %+v", got, want)
	}
	if !got.SourceMTime.Equal(want.SourceMTime) {
		t.Errorf("mtime: got %s want %s", got.SourceMTime, want.SourceMTime)
	}
}

// The pre-filter compares stored mtimes for exact equality, so the precision matters: dropping
// sub-second digits makes that branch unreachable and re-reads every file forever.
func TestAStoredMTimeKeepsTheNanosecondsThePreFilterComparesOn(t *testing.T) {
	dir := t.TempDir()
	fp := fingerprint()
	if fp.SourceMTime.Nanosecond() == 0 {
		t.Fatal("the fixture has a whole-second mtime; this test would pass vacuously")
	}

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fp); err != nil {
		t.Fatal(err)
	}
	s.Close()

	got, ok := open(t, dir).Get(key("/x/a.jsonl"))
	if !ok {
		t.Fatal("entry did not survive a reload")
	}
	if !got.SourceMTime.Equal(fp.SourceMTime) {
		t.Errorf("mtime lost precision across the round trip:\n got %s\nwant %s",
			got.SourceMTime.Format(time.RFC3339Nano), fp.SourceMTime.Format(time.RFC3339Nano))
	}
}

// The flock stops two flushes interleaving, non-blocking: a second flush is told the store is busy.
func TestSecondOpenIsRefusedNotQueued(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	defer first.Close()

	if _, err := engine.Open(dir, installID); !errors.Is(err, engine.ErrLocked) {
		t.Fatalf("a second open must return ErrLocked, got %v", err)
	}
}

func TestLockIsReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	s, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	s2.Close()
}

// status and doctor must not contend with a flush, so Peek takes no lock.
func TestPeekWorksWhileLocked(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}

	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("Peek must work while the store is locked: %v", err)
	}
	if len(doc.Entries) != 1 {
		t.Errorf("Peek saw %d entries, want 1", len(doc.Entries))
	}
	if doc.InstallID != installID {
		t.Errorf("Peek install_id: %q", doc.InstallID)
	}
}

// A document from a different version is rejected: a wiped store re-uploads onto existing keys.
func TestForeignSchemaIsRejected(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	path := filepath.Join(dir, engine.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(raw), `"state_schema": 1`, `"state_schema": 2`, 1)
	if bumped == string(raw) {
		t.Fatal("could not bump state_schema; document shape changed")
	}
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Open(dir, installID); err == nil {
		t.Fatal("a foreign state_schema must be rejected, never guessed at")
	}
}

func TestCorruptDocumentIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(dir, installID); err == nil {
		t.Fatal("a corrupt document must be rejected")
	}
}

// The failure the checksum exists for: a source_hash flipped in place still parses, still
// satisfies the schema, and reads as a completed ship. Trusting it loses that file forever.
func TestAnInPlaceCorruptionIsCaughtByTheChecksum(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	path := filepath.Join(dir, engine.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), sha, otherSha, 1)
	if tampered == string(raw) {
		t.Fatal("could not tamper with the source hash; document shape changed")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("a corrupt store must not fail the run: %v", err)
	}
	if !doc.Corrupt {
		t.Error("the tampered document was trusted")
	}
	if len(doc.Entries) != 0 {
		t.Errorf("a document that failed its checksum must be discarded whole, kept %d entries", len(doc.Entries))
	}
}

// Discarding is not erroring: the next run re-ships onto the keys that already exist, which is the
// cost the store's design already accepts. Refusing to run would turn this into an outage.
func TestACorruptStoreStillCollects(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	path := filepath.Join(dir, engine.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), sha, otherSha, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	s2, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("open over a corrupt document must succeed: %v", err)
	}
	defer s2.Close()
	if _, ok := s2.Get(key("/x/a.jsonl")); ok {
		t.Error("a discarded document must not still answer for its entries")
	}
}

// A document from before the field existed has no checksum and must still load; it earns one on
// the next rewrite rather than being treated as corrupt.
func TestADocumentWithoutAChecksumStillLoads(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": []}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("a pre-checksum document must load: %v", err)
	}
	if got.Corrupt {
		t.Error("an absent checksum is not a failed one")
	}

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"checksum"`) {
		t.Error("the rewrite should have stamped a checksum")
	}
}

// An unknown field is ignored and dropped on the next rewrite; absence fails toward re-shipping.
func TestUnknownFieldIsIgnored(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": [], "pending_uploads": []}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("a document with an unknown field must still load: %v", err)
	}
	s.Close()
}

// The one exception to load's interpret-don't-audit stance: a negative attempts count reaches the
// backoff as a negative shift and panics, so the document is refused with the entry named.
func TestNegativeAttemptsIsRejectedOnLoad(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": [{"source_id": "claude-code-transcripts", "native_path": "/x/a.jsonl", "attempts": -5}]}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Peek(dir)
	if err == nil {
		t.Fatal("a negative attempts count must be rejected")
	}
	if !strings.Contains(err.Error(), "attempts") || !strings.Contains(err.Error(), "/x/a.jsonl") {
		t.Errorf("the error must name the value and the entry, got: %v", err)
	}
}

// Load interprets, it does not audit. A missing field comes back zero, which matches no file, so
// the engine re-reads and re-ships onto the same key: the safe direction to fail in.
func TestMissingFieldsLoadAsZeroValues(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": [{"source_id": "claude-code-transcripts", "native_path": "/x/a.jsonl"}]}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("a well-formed document must load: %v", err)
	}
	k := engine.Key{SourceID: "claude-code-transcripts", NativePath: "/x/a.jsonl"}
	fp, ok := loaded.Entries[k]
	if !ok {
		t.Fatalf("entry missing; document loaded as %+v", loaded.Entries)
	}
	if fp.SourceHash != "" || fp.SourceSize != 0 || !fp.SourceMTime.IsZero() {
		t.Errorf("absent fields must read back zero, got %+v", fp)
	}
}

// A document carrying the retired sink_etag must load and simply drop the field: the schema is
// not bumped for a removal, and refusing would re-ship the install's whole history.
func TestALegacySinkETagLoadsAndIsDropped(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": [{"source_id": "claude-code-transcripts", ` +
		`"native_path": "/x/a.jsonl", "source_hash": "` + sha + `", "sink_etag": "abc123"}]}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("a document carrying sink_etag must still load: %v", err)
	}
	k := engine.Key{SourceID: "claude-code-transcripts", NativePath: "/x/a.jsonl"}
	fp, ok := loaded.Entries[k]
	if !ok {
		t.Fatalf("entry missing; document loaded as %+v", loaded.Entries)
	}
	if fp.SourceHash != sha {
		t.Errorf("the entry beside the dropped field was lost: %+v", fp)
	}

	// The rewrite drops it: the schema refuses unknown properties on the way out.
	s := open(t, dir)
	if err := commit(s, k, fingerprint()); err != nil {
		t.Fatalf("rewriting a document that carried sink_etag: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sink_etag") {
		t.Errorf("sink_etag survived a rewrite:\n%s", raw)
	}
}

// The schema is enforced on the way out, so a bad commit fails and the old document stays.
func TestCommitOfAnUnserializableEntryFails(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}

	bad := engine.Key{
		SourceID:   "claude-code-transcripts",
		NativePath: "",
	}
	if err := commit(s, bad, fingerprint()); err == nil {
		t.Fatal("a document that would not satisfy its own schema must not be written")
	}

	after, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the rejected commit reached the disk")
	}
}

// A state file with the wrong identity would attribute another install's uploads to this one.
func TestDocumentFromAnotherInstallIsRejected(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := engine.Open(dir, "11111111-2222-3333-4444-555555555555"); err == nil {
		t.Fatal("a document belonging to another install must be refused")
	}
}

// A crash before the atomic replace leaves the previous document intact: retry is re-run.
func TestCrashBeforeCommitLeavesThePreviousDocument(t *testing.T) {
	dir := t.TempDir()

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	before, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}

	// A second store mutates in memory and is abandoned without committing.
	s2 := open(t, dir)
	s2.Get(key("/x/a.jsonl"))
	s2.Close()

	after, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the document changed without a commit")
	}
}

// The document is deterministic, so a diff shows real change rather than map ordering.
func TestDocumentIsDeterministic(t *testing.T) {
	write := func(dir string, paths []string) string {
		s, err := engine.Open(dir, installID)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for _, p := range paths {
			if err := commit(s, key(p), fingerprint()); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(filepath.Join(dir, engine.FileName))
		if err != nil {
			t.Fatal(err)
		}
		// updated_at moves per flush, and checksum covers it; strip both so the comparison is
		// about ordering.
		var out []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "updated_at") && !strings.Contains(line, "checksum") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}

	forward := write(t.TempDir(), []string{"/x/a.jsonl", "/x/b.jsonl", "/x/c.jsonl"})
	reverse := write(t.TempDir(), []string{"/x/c.jsonl", "/x/b.jsonl", "/x/a.jsonl"})
	if forward != reverse {
		t.Errorf("insertion order changed the document:\n%s\n---\n%s", forward, reverse)
	}
}

// A spec change means the source is read differently, so only its entries drop.
func TestEnsureSpecDropsOnlyTheChangedSource(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)

	for _, id := range []string{"claude-code-transcripts", "codex-rollouts"} {
		if _, err := s.EnsureSpec(id, sha); err != nil {
			t.Fatal(err)
		}
	}
	entries := []engine.Key{
		{SourceID: "claude-code-transcripts", NativePath: "/x/a.jsonl"},
		{SourceID: "claude-code-transcripts", NativePath: "/x/b.jsonl"},
		{SourceID: "codex-rollouts", NativePath: "/y/r.jsonl"},
	}
	for _, k := range entries {
		if err := commit(s, k, fingerprint()); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := s.EnsureSpec("claude-code-transcripts", sha); err != nil || n != 0 {
		t.Fatalf("an unchanged spec must drop nothing: n=%d err=%v", n, err)
	}
	if n, err := s.EnsureSpec("claude-code-transcripts", otherSha); err != nil || n != 2 {
		t.Fatalf("a changed spec must drop that source's entries: n=%d err=%v", n, err)
	}
	if s.Len() != 1 {
		t.Errorf("expected 1 surviving entry, got %d", s.Len())
	}
	if _, ok := s.Get(entries[2]); !ok {
		t.Error("an unrelated source must not be touched")
	}
}

// An older document carries the generation per entry: load seeds source_specs from unanimous
// entries, so the upgrade re-ships nothing and a later spec change still drops.
func TestLegacyPerEntrySpecSeedsSourceSpecs(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "entries": [
	  {"source_id": "claude-code-transcripts", "native_path": "/x/a.jsonl", "source_spec_fingerprint": "` + sha + `"},
	  {"source_id": "claude-code-transcripts", "native_path": "/x/b.jsonl", "source_spec_fingerprint": "` + sha + `"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir)
	if n, err := s.EnsureSpec("claude-code-transcripts", sha); err != nil || n != 0 {
		t.Fatalf("the seeded generation matches, so nothing may drop: n=%d err=%v", n, err)
	}
	if s.Len() != 2 {
		t.Errorf("expected both legacy entries to survive, got %d", s.Len())
	}
	if n, err := s.EnsureSpec("claude-code-transcripts", otherSha); err != nil || n != 2 {
		t.Fatalf("a real spec change after the upgrade must still drop: n=%d err=%v", n, err)
	}
}

// A derived entry carries the enricher that produced it and its output hash.
func TestDerivedEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)

	k := engine.Key{
		SourceID:   "cursor-transcripts",
		NativePath: "/c/c8cbeb0b.jsonl.enriched.jsonl",
	}
	fp := fingerprint()
	fp.Enricher = &engine.EnricherRef{ID: "cursor-transcript-join", Version: 1}
	fp.OutputHash = otherSha
	if err := commit(s, k, fp); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := open(t, dir)
	got, ok := s2.Get(k)
	if !ok {
		t.Fatal("derived entry missing after reload")
	}
	if got.Enricher == nil || got.Enricher.ID != "cursor-transcript-join" || got.Enricher.Version != 1 {
		t.Errorf("enricher ref: %+v", got.Enricher)
	}
	if got.OutputHash != otherSha {
		t.Errorf("output hash: %q", got.OutputHash)
	}
}

func TestCommitAllReplacesOnce(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)

	updates := map[engine.Key]engine.Fingerprint{
		key("/x/a.jsonl"): fingerprint(),
		key("/x/b.jsonl"): fingerprint(),
		key("/x/c.jsonl"): fingerprint(),
	}
	if err := s.CommitAll(updates); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Errorf("expected 3 entries, got %d", s.Len())
	}
	s.Close()

	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 3 {
		t.Errorf("expected 3 entries on disk, got %d", len(doc.Entries))
	}
}

// A wiped document is cheap: the store comes back empty and re-uploads onto existing keys.
func TestWipedDocumentComesBackEmpty(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := os.Remove(filepath.Join(dir, engine.FileName)); err != nil {
		t.Fatal(err)
	}
	s2, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("a wiped document must not be an error: %v", err)
	}
	defer s2.Close()
	if s2.Len() != 0 {
		t.Errorf("expected an empty store, got %d entries", s2.Len())
	}
}

// Reset is the one-command full re-ship onto existing keys; its dry run must not write.
func TestResetForgetsEverythingButOnlyWithApply(t *testing.T) {
	dir := t.TempDir()

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	if err := commit(s, key("/x/b.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	removed, err := engine.Reset(dir, installID, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("dry run should count both entries, counted %d", removed)
	}
	s2 := open(t, dir)
	if s2.Len() != 2 {
		t.Fatalf("a dry run wrote: %d entries remain, want 2", s2.Len())
	}
	s2.Close()

	removed, err = engine.Reset(dir, installID, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("apply should count what it forgot, counted %d", removed)
	}
	s3 := open(t, dir)
	defer s3.Close()
	if s3.Len() != 0 {
		t.Errorf("apply left %d entries", s3.Len())
	}
}

// A reset store keeps its install id, so a later open under another identity is still refused.
func TestResetKeepsTheInstallID(t *testing.T) {
	dir := t.TempDir()

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := engine.Reset(dir, installID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(dir, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("a reset document lost its install id: a foreign install opened it")
	}
}

// Resetting an empty store is a no-op, not an error: it must be safe to run fleet-wide.
func TestResetOnAnEmptyStoreIsANoOp(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Close()

	removed, err := engine.Reset(dir, installID, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("an empty store forgot %d entries", removed)
	}
}

// Reset contends like every verb: told the store is busy, never queued behind it.
func TestResetIsRefusedWhileLocked(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()

	if _, err := engine.Reset(dir, installID, false); !errors.Is(err, engine.ErrLocked) {
		t.Errorf("reset under a held lock: err = %v, want ErrLocked", err)
	}
}

// A kill -9 mid-flush strands a fingerprints temp. The old fixed name, fingerprints.json.tmp-<pid>,
// wedged every commit once a pid recurred: O_EXCL refused the name until a human deleted the file,
// and every run re-shipped everything. Temp names are random now; this pins the wedge closed.
func TestAStrandedOwnPidTempDoesNotWedgeTheCommit(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, engine.FileName+fmt.Sprintf(".tmp-%d", os.Getpid()))
	if err := os.WriteFile(tmp, []byte(`{"half":"written`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir)
	if err := commit(s, key("/x/a.jsonl"), fingerprint()); err != nil {
		t.Fatalf("a stranded temp from a crashed run wedged the commit: %v", err)
	}
	s.Close()

	s2 := open(t, dir)
	if _, ok := s2.Get(key("/x/a.jsonl")); !ok {
		t.Fatal("the committed entry did not survive a reload")
	}
}
