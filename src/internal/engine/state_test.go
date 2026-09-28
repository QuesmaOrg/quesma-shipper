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
const otherInstall = "85a7e04c-32a4-4bf5-9c80-49c4f9d087bb"

// What a re-enrolled machine wakes up to: a document another install left behind.
func seedForeignDoc(t *testing.T, dir string, entries int) {
	t.Helper()
	s, err := engine.Open(dir, otherInstall)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnsureSpec("claude-code-transcripts", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < entries; i++ {
		if err := commit(s, key(fmt.Sprintf("/x/%d.jsonl", i)), fingerprint()); err != nil {
			t.Fatal(err)
		}
	}
}

func key(path string) engine.Key {
	return engine.Key{SourceID: "claude-code-transcripts", ID: path}
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
	if s.Corrupt() {
		t.Error("a missing document is a first run, not a discarded one")
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

	if _, err := engine.Peek(dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("the tampered document was trusted: %v", err)
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
	if _, err := engine.Peek(dir); err != nil {
		t.Fatalf("a pre-checksum document must load: %v", err)
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
	k := engine.Key{SourceID: "claude-code-transcripts", ID: "/x/a.jsonl"}
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
	k := engine.Key{SourceID: "claude-code-transcripts", ID: "/x/a.jsonl"}
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

// A document an earlier binary wrote, checksum included, loads with its path as the key: logical
// identity is additive, so no install loses its fingerprints to the upgrade.
func TestAPreIdentityDocumentLoadsKeyedByPath(t *testing.T) {
	dir := t.TempDir()
	doc := `{"state_schema": 1, "install_id": "` + installID + `", "updated_at": "2026-09-01T00:00:00Z",
  "source_specs": {"claude-code-transcripts": "` + sha + `"},
  "checksum": "438d9b39765294da524980b655a90f70d63d63407fc32b1fe2a883212b35410c",
  "entries": [{"source_id": "claude-code-transcripts", "native_path": "/x/a.jsonl", "source_size": 4096,
    "source_mtime": "2026-07-30T10:00:00.987654321Z", "source_hash": "` + sha + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := engine.Peek(dir)
	if err != nil {
		t.Fatalf("a pre-identity document must load: %v", err)
	}
	fp, ok := loaded.Entries[key("/x/a.jsonl")]
	if !ok {
		t.Fatalf("entry is not keyed by its path; document loaded as %+v", loaded.Entries)
	}
	if fp.SourceHash != sha || fp.NativePath != "" {
		t.Errorf("a path-keyed entry loaded as %+v", fp)
	}
}

// An older binary drops the identity field it does not know, so an entry it could read must not
// carry one: path-keyed entries keep their exact pre-identity bytes.
func TestAPathKeyedEntryWritesNoIdentity(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	fp := fingerprint()
	fp.NativePath = "/x/a.jsonl"
	if err := commit(s, key("/x/a.jsonl"), fp); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"identity"`) {
		t.Errorf("a path-keyed entry carries an identity:\n%s", raw)
	}
}

// A logical identity survives a reload with the path it was last seen at, which is what Prune tests.
func TestAnIdentityEntryRoundTripsWithItsObservedPath(t *testing.T) {
	dir := t.TempDir()
	path := "/h/.codex/archived_sessions/rollout-2026-09-01T10-00-00-0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl"
	k := engine.Key{SourceID: "codex-rollouts", ID: "codex-session/0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"}
	fp := fingerprint()
	fp.NativePath = path

	s := open(t, dir)
	if err := commit(s, k, fp); err != nil {
		t.Fatal(err)
	}
	s.Close()

	raw, err := os.ReadFile(filepath.Join(dir, engine.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"native_path": "` + path + `"`, `"identity": "` + k.ID + `"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document lacks %s:\n%s", want, raw)
		}
	}

	got, ok := open(t, dir).Get(k)
	if !ok {
		t.Fatal("an identity entry did not survive a reload")
	}
	if got.NativePath != path || got.SourceHash != sha {
		t.Errorf("identity entry reloaded as %+v", got)
	}
}

// Prune tests the last observed path of an identity entry, never its identity, which is not a path.
func TestPruneTestsTheObservedPathOfAnIdentityEntry(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(t.TempDir(), "rollout-a.jsonl.zst")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alive := engine.Key{SourceID: "codex-rollouts", ID: "codex-session/a"}
	gone := engine.Key{SourceID: "codex-rollouts", ID: "codex-session/b"}
	aliveFP, goneFP := fingerprint(), fingerprint()
	aliveFP.NativePath = present
	goneFP.NativePath = filepath.Join(filepath.Dir(present), "rollout-b.jsonl")

	s := open(t, dir)
	if err := s.CommitAll(map[engine.Key]engine.Fingerprint{alive: aliveFP, gone: goneFP}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	removed, kept, err := engine.Prune(dir, installID, false)
	if err != nil || removed != 1 || kept != 1 {
		t.Fatalf("prune: removed=%d kept=%d err=%v, want 1 and 1", removed, kept, err)
	}
	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Entries[alive]; !ok {
		t.Error("prune dropped an identity entry whose file exists")
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

	if err := commit(s, key(""), fingerprint()); err == nil {
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
		{SourceID: "claude-code-transcripts", ID: "/x/a.jsonl"},
		{SourceID: "claude-code-transcripts", ID: "/x/b.jsonl"},
		{SourceID: "codex-rollouts", ID: "/y/r.jsonl"},
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

// A derived entry carries the enricher that produced it and its output hash.
func TestDerivedEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)

	k := engine.Key{
		SourceID: "cursor-transcripts",
		ID:       "/c/c8cbeb0b.jsonl.enriched.jsonl",
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

// A reset store keeps its install id, so a later open under another identity still discards it.
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
	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.InstallID != installID {
		t.Errorf("a reset document lost its install id: %q", doc.InstallID)
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

// An unloadable document is what an operator runs reset against, so --apply must replace it even
// though the discarded store forgot nothing.
func TestResetReplacesAnUnloadableDocument(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, engine.FileName), []byte("not a document\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reset(dir, installID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Peek(dir); err != nil {
		t.Fatalf("the document was not replaced: %v", err)
	}
}

// Re-enrolling replaces identity.json and leaves the old install's document behind. Its entries
// name objects under that install's key root, so not one of them may survive into this install.
func TestAnotherInstallsEntriesNeverSurvive(t *testing.T) {
	dir := t.TempDir()
	seedForeignDoc(t, dir, 3)

	s, err := engine.Open(dir, installID)
	if err != nil {
		t.Fatalf("a foreign document must be discarded, not refused: %v", err)
	}
	if !s.Corrupt() {
		t.Error("the discard was not reported to the run")
	}
	if s.Len() != 0 {
		t.Fatalf("%d of another install's entries survived", s.Len())
	}
	if err := commit(s, key("/x/mine.jsonl"), fingerprint()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.InstallID != installID {
		t.Errorf("the replacement kept the foreign install id %q", doc.InstallID)
	}
	if doc.ForeignTo(installID) {
		t.Error("the replacement still reads as foreign, so the next run discards it again")
	}
	if len(doc.Entries) != 1 {
		t.Errorf("the replacement holds %d entries, want only this install's one", len(doc.Entries))
	}
}

// Prune keeps entries, so it is the verb that could claim another install's uploads. It cannot:
// the discard empties the store before pruning ever looks at it.
func TestPruneCannotClaimAnotherInstallsUploads(t *testing.T) {
	dir := t.TempDir()
	seedForeignDoc(t, dir, 2)

	removed, kept, err := engine.Prune(dir, installID, false)
	if err != nil {
		t.Fatalf("prune over a foreign document: %v", err)
	}
	if removed != 0 || kept != 0 {
		t.Errorf("prune reported removed=%d kept=%d over a discarded store, want 0 and 0", removed, kept)
	}
	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 0 || doc.InstallID != installID {
		t.Errorf("prune left %d entries under install %q", len(doc.Entries), doc.InstallID)
	}
}

// An entryless foreign document forgets nothing, so only the install id makes it a replacement.
// Reset must still write it, or the stale id survives and every run re-discards the file.
func TestResetReplacesAnEmptyForeignDocument(t *testing.T) {
	dir := t.TempDir()
	seedForeignDoc(t, dir, 0)

	removed, err := engine.Reset(dir, installID, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("an entryless document forgot %d entries", removed)
	}
	doc, err := engine.Peek(dir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.InstallID != installID {
		t.Errorf("reset left the foreign install id %q in place", doc.InstallID)
	}
}

// A dry run reports; it never writes. The in-memory discard must not reach the file.
func TestADryRunLeavesAnUnloadableDocumentOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, engine.FileName)
	original := []byte("not a document\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, run := range []struct {
		name string
		fn   func() error
	}{
		{"reset", func() error { _, err := engine.Reset(dir, installID, true); return err }},
		{"prune", func() error { _, _, err := engine.Prune(dir, installID, true); return err }},
	} {
		if err := run.fn(); err != nil {
			t.Fatalf("%s dry run: %v", run.name, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(original) {
			t.Errorf("%s dry run rewrote the document: %q", run.name, got)
		}
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
