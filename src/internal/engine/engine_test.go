package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// --- harness ----------------------------------------------------------------

type fixture struct {
	t        *testing.T
	home     string
	stateDir string
	store    *engine.Store
	port     *fakePort
	unit     *identity.Unit
	eff      *config.Effective
	log      *auditlog.Log
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	home := t.TempDir()
	stateDir := filepath.Join(home, "state")

	unit, err := identity.Mint(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	log, err := auditlog.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{
		t:        t,
		home:     home,
		stateDir: stateDir,
		port:     newPort(),
		unit:     unit,
		log:      log,
	}
	f.eff = f.effective(nil)
	f.reopen()
	return f
}

// effective builds a resolved config over a Claude-Code-shaped store, skipping the file layers.
func (f *fixture) effective(extraSources []sources.Resolved) *config.Effective {
	root := filepath.Join(f.home, ".claude")
	src := sources.Resolved{
		Source: sources.Source{
			ID:            "claude-code-transcripts",
			Family:        "claude-code",
			Gather:        "file_glob",
			ArtifactClass: "trajectory",
			Include:       []string{"projects/**/*.jsonl"},
			Sniff:         &sources.Sniff{Kind: "jsonl", MaxScanBytes: 65536},
		},
		Root:            root,
		Enabled:         true,
		SpecFingerprint: strings.Repeat("a", 64),
	}
	return &config.Effective{
		ConfigVersion:  1,
		MaxFilesPerRun: 64,
		StateDir:       f.stateDir,
		RulePacks:      []string{"gitleaks-core", "quesma-extra", "cloud-keys", "generic-entropy", "pii-core"},
		StructuralEx: map[string][]string{
			"claude-code": {"uuid", "parentUuid", "sessionId"},
			"*":           {"timestamp", "version"},
		},
		Sources:    append([]sources.Resolved{src}, extraSources...),
		Deny:       sources.New(f.home),
		Provenance: map[string]config.Origin{},
	}
}

func (f *fixture) reopen() {
	f.t.Helper()
	if f.store != nil {
		f.store.Close()
	}
	s, err := engine.Open(f.stateDir, f.unit.InstallID.String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = s
	f.t.Cleanup(func() { s.Close() })
}

func (f *fixture) opts() engine.Options {
	return engine.Options{
		Plan:       planOf(f.eff),
		Identity:   f.unit,
		Upload:     f.port,
		Log:        f.log,
		Registry:   sources.NewRegistry(),
		Recipients: []age.Recipient{f.unit.Recipient()},
		Now:        func() time.Time { return time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC) },
		Client:     transforms.Client{Version: "0.1.0-test"},
	}
}

// countStateWrites reports how many times the state document was replaced while fn ran.
func (f *fixture) countStateWrites(fn func()) int {
	f.t.Helper()
	path := filepath.Join(f.stateDir, "fingerprints.json")
	seen := map[string]bool{}
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		for {
			select {
			case <-stop:
				done <- len(seen)
				return
			default:
			}
			if b, err := os.ReadFile(path); err == nil {
				seen[string(b)] = true
			}
			time.Sleep(time.Millisecond)
		}
	}()
	fn()
	close(stop)
	return <-done
}

// runWith is run() with the options adjusted, for the settings only a test sets.
func (f *fixture) runWith(adjust func(*engine.Options)) engine.Report {
	f.t.Helper()
	o := f.opts()
	adjust(&o)
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return rep
}

// wipeState is state loss: the fingerprint file gone, everything on the store intact.
func (f *fixture) wipeState() {
	f.t.Helper()
	f.store.Close()
	if err := os.Remove(filepath.Join(f.stateDir, engine.FileName)); err != nil {
		f.t.Fatal(err)
	}
	f.reopen()
}

func (f *fixture) run() engine.Report {
	f.t.Helper()
	rep, err := engine.Run(context.Background(), f.store, f.opts())
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return rep
}

// runDry is preview: read, scrub and seal, but neither upload nor commit.
func (f *fixture) runDry() engine.Report {
	f.t.Helper()
	o := f.opts()
	o.DryRun = true
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		f.t.Fatalf("preview: %v", err)
	}
	return rep
}

// runUnbounded is the drain: max_files_per_run does not apply.
func (f *fixture) runUnbounded() engine.Report {
	f.t.Helper()
	o := f.opts()
	o.Unbounded = true
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		f.t.Fatalf("drain: %v", err)
	}
	return rep
}

// engineFixedTime is the fixture's clock, so pause timestamps are not wall-clock dependent.
func engineFixedTime() time.Time { return time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC) }

func (f *fixture) writeTranscript(rel, body string) string {
	f.t.Helper()
	path := filepath.Join(f.home, ".claude", "projects", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) appendTranscript(rel, body string) {
	f.t.Helper()
	path := filepath.Join(f.home, ".claude", "projects", rel)
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(body); err != nil {
		f.t.Fatal(err)
	}
}

const line1 = `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/Users/jane/work/api","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"
const line2 = `{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s1","message":{"content":[{"type":"text","text":"world"}]}}` + "\n"

// --- the core loop ----------------------------------------------------------

func TestCollectsAndShips(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)

	rep := f.run()
	if rep.Shipped != 1 {
		t.Fatalf("expected 1 shipped, got %+v", rep)
	}
	if len(f.port.keys()) != 1 {
		t.Fatalf("expected 1 object, got %v", f.port.keys())
	}

	// The object must be a real sealed container that opens with this install's identity.
	obj, _ := f.port.get(f.port.keys()[0])
	m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
	if err != nil {
		t.Fatalf("the shipped object must open: %v", err)
	}
	if !strings.Contains(m.NativePath, "s1.jsonl") {
		t.Errorf("manifest native_path: %q", m.NativePath)
	}
	if !strings.Contains(string(payload), `"uuid":"u1"`) {
		t.Errorf("payload lost its identifiers: %s", payload)
	}
	if m.ArtifactClass != "trajectory" {
		t.Errorf("artifact class %q", m.ArtifactClass)
	}
	if obj.Metadata["artifact-class"] != "trajectory" {
		t.Errorf("artifact-class metadata %q — the classification must be readable without a decrypt", obj.Metadata["artifact-class"])
	}
}

// Appending two lines re-ships the file whole onto the SAME key, adding a version.
func TestAppendTwoLinesOverwritesTheSameKey(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)

	f.run()
	keysAfterFirst := f.port.keys()
	if len(keysAfterFirst) != 1 {
		t.Fatalf("expected 1 key, got %v", keysAfterFirst)
	}

	f.appendTranscript("p/s1.jsonl", line2)
	rep := f.run()

	if rep.Shipped != 1 {
		t.Fatalf("the grown file should re-ship: %+v", rep)
	}
	if got := f.port.keys(); len(got) != 1 || got[0] != keysAfterFirst[0] {
		t.Errorf("keys changed: %v, want %v", got, keysAfterFirst)
	}
	obj, _ := f.port.get(keysAfterFirst[0])
	if obj.Versions != 2 {
		t.Errorf("expected version 2, got %d — history is noncurrent versions", obj.Versions)
	}

	// Both lines must be present: the whole file ships, not a delta.
	_, payload, err := transforms.Open(obj.Body, f.unit.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"uuid":"u1"`) || !strings.Contains(string(payload), `"uuid":"a1"`) {
		t.Errorf("the re-shipped object is not the whole file: %s", payload)
	}
}

// Touching mtime without changing bytes must re-ship nothing: the content hash is the authority.
func TestMTimeTouchDoesNotReship(t *testing.T) {
	f := newFixture(t)
	paths := []string{"p/a.jsonl", "p/b.jsonl", "p/c.jsonl"}
	for _, p := range paths {
		f.writeTranscript(p, line1)
	}

	first := f.run()
	if first.Shipped != 3 {
		t.Fatalf("expected 3 shipped, got %+v", first)
	}
	f.port.reset()

	// Rewrite identical content with a new mtime, as the MCP descriptors do on a live Cursor store.
	later := time.Now().Add(time.Hour)
	for _, p := range paths {
		full := filepath.Join(f.home, ".claude", "projects", p)
		if err := os.WriteFile(full, []byte(line1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, later, later); err != nil {
			t.Fatal(err)
		}
	}

	second := f.run()
	if second.Shipped != 0 {
		t.Errorf("nothing should re-ship on an mtime-only change, got %d", second.Shipped)
	}
	if second.Unchanged != 3 {
		t.Errorf("expected 3 unchanged, got %+v", second)
	}
	if writes := f.port.putCount(); writes != 0 {
		t.Errorf("%d sink writes for an mtime-only change", writes)
	}
}

// The pre-filter must NOT read a file whose stat has not moved: "size and mtime unchanged" means
// never opened. The mtime carries nanoseconds: a whole-second fixture cannot see the precision bug.
func TestAnUnchangedFileIsNotReadTwice(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)

	full := filepath.Join(f.home, ".claude", "projects", "p/a.jsonl")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	if err := os.Chtimes(full, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := statMTime(t, full); got.Nanosecond() == 0 {
		t.Skipf("this filesystem stores whole-second mtimes (%s); the pre-filter cannot be "+
			"distinguished from the hash path here", got)
	}

	if first := f.run(); first.Shipped != 1 {
		t.Fatalf("expected 1 shipped, got %+v", first)
	}

	// Reopened, because that is what the next tick is: in one process the bug is invisible.
	f.reopen()

	second := f.run()
	if second.Unchanged != 1 {
		t.Fatalf("expected 1 unchanged, got %+v", second)
	}

	// Read off the run's own outcome: per-file unchanged entries are elided from the audit log.
	var reason string
	for _, s := range second.Sources {
		for _, fo := range s.Files {
			if fo.Decision == auditlog.DecisionUnchanged {
				reason = fo.Reason
			}
		}
	}
	if reason != "size and mtime unchanged" {
		t.Errorf("the file was re-read: the unchanged decision says %q, want "+
			"\"size and mtime unchanged\"", reason)
	}
}

// Per-file unchanged entries would grow the log at scan rate, so one aggregate replaces them.
func TestUnchangedFilesAuditAsOneAggregateEntry(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)
	f.writeTranscript("p/b.jsonl", line2)
	f.writeTranscript("p/c.jsonl", line1)

	if first := f.run(); first.Shipped != 3 {
		t.Fatalf("expected 3 shipped, got %+v", first)
	}
	f.reopen()
	if second := f.run(); second.Unchanged != 3 {
		t.Fatalf("expected 3 unchanged, got %+v", second)
	}

	entries, err := auditlog.Tail(f.log.Path(), 0)
	if err != nil {
		t.Fatal(err)
	}
	perFile, aggregates := 0, 0
	var reason string
	for _, e := range entries {
		if e.Decision != auditlog.DecisionUnchanged {
			continue
		}
		if e.File != "" {
			perFile++
		} else {
			aggregates++
			reason = e.Reason
		}
	}
	if perFile != 0 {
		t.Errorf("%d per-file unchanged entries; the elision is not happening", perFile)
	}
	if aggregates != 1 {
		t.Fatalf("%d aggregate unchanged entries, want exactly 1", aggregates)
	}
	if want := "3 files unchanged by size and mtime, not opened; per-file entries elided"; reason != want {
		t.Errorf("aggregate reason %q, want %q", reason, want)
	}
}

// Only files the run never opened are elided; one it READ keeps its per-file entry.
func TestAReadButUnchangedFileKeepsItsPerFileAuditEntry(t *testing.T) {
	f := newFixture(t)
	path := f.writeTranscript("p/a.jsonl", line1)
	f.writeTranscript("p/b.jsonl", line2)

	if first := f.run(); first.Shipped != 2 {
		t.Fatalf("expected 2 shipped, got %+v", first)
	}

	// A new mtime under identical bytes: the pre-filter cannot clear it, so the run reads it.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if second := f.run(); second.Unchanged != 2 {
		t.Fatalf("expected 2 unchanged, got %+v", second)
	}

	entries, err := auditlog.Tail(f.log.Path(), 0)
	if err != nil {
		t.Fatal(err)
	}
	perFile, aggregates := 0, 0
	for _, e := range entries {
		if e.Decision != auditlog.DecisionUnchanged {
			continue
		}
		if e.File == "" {
			aggregates++
			continue
		}
		perFile++
		if e.File != path {
			t.Errorf("per-file unchanged entry for %s, want %s", e.File, path)
		}
		if e.BytesIn == 0 {
			t.Error("the per-file entry must carry the bytes the run read")
		}
	}
	if perFile != 1 {
		t.Errorf("%d per-file unchanged entries, want exactly 1: the file the run read", perFile)
	}
	if aggregates != 1 {
		t.Errorf("%d aggregate entries, want 1: the file the pre-filter passed over", aggregates)
	}
}

func statMTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// A crash between the upload and the commit re-runs the file onto the same key. Zero new keys.
func TestCrashBeforeCommitReRunsOntoTheSameKey(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)

	// The lost commit is simulated by wiping state after a successful upload.
	f.run()
	keys := f.port.keys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %v", keys)
	}

	f.wipeState()
	f.port.reset()

	rep := f.run()
	if rep.Shipped != 1 {
		t.Fatalf("the file should be re-run: %+v", rep)
	}
	if got := f.port.keys(); len(got) != 1 || got[0] != keys[0] {
		t.Errorf("re-run created a new key: %v, want %v", got, keys)
	}
	obj, _ := f.port.get(keys[0])
	if obj.Versions != 2 {
		t.Errorf("expected the re-run to add a version, got %d", obj.Versions)
	}
}

// Wiping the fingerprint document while keeping the identity converges: ZERO new keys.
func TestWipedStateConvergesWithZeroNewKeys(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"p/a.jsonl", "p/b.jsonl", "p/c.jsonl"} {
		f.writeTranscript(p, line1)
	}

	f.run()
	before := f.port.keys()
	if len(before) != 3 {
		t.Fatalf("expected 3 keys, got %v", before)
	}

	f.wipeState()

	rep := f.run()
	if rep.Shipped != 3 {
		t.Errorf("everything should re-upload once: %+v", rep)
	}
	after := f.port.keys()
	if len(after) != len(before) {
		t.Fatalf("the bucket gained keys: %v, want %v", after, before)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("key %d changed: %s -> %s", i, before[i], after[i])
		}
	}
}

// A failed upload commits nothing, so the next tick re-runs the file.
func TestFailedUploadCommitsNothing(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)

	f.port.FailNext = errors.New("network is unreachable")
	rep := f.run()
	if rep.Failed != 1 {
		t.Fatalf("expected a failure, got %+v", rep)
	}
	if len(f.port.keys()) != 0 {
		t.Error("nothing should have landed")
	}

	rep2 := f.run()
	if rep2.Shipped != 1 {
		t.Errorf("the next tick must re-run the file: %+v", rep2)
	}
}

// --- redaction and never-mutate ---------------------------------------------

// The payload is scrubbed before sealing, and the identifiers that make it a graph survive.
func TestPayloadIsScrubbedAndTheGraphSurvives(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl",
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s1","cwd":"/Users/jane/work",`+
			`"message":{"content":[{"type":"text","text":"key ghp_abcdefghijklmnopqrstuvwxyz0123456789"}]}}`+"\n")

	f.run()
	obj, _ := f.port.get(f.port.keys()[0])
	m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(payload), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Error("the secret reached the sink")
	}
	if !strings.Contains(string(payload), `"uuid":"u1"`) {
		t.Error("an exempt identifier was redacted; the graph would not reassemble")
	}
	if m.Redaction == nil || m.Redaction.RuleHits["github-pat"] != 1 {
		t.Errorf("the redaction ledger should record the rule: %+v", m.Redaction)
	}
	if m.Redaction.Density <= 0 {
		t.Error("density should be recorded: it is the rule-drift alarm")
	}
}

// A full cycle must not write anything under the agent's directory: no sidecars, no temp files.
func TestNeverMutatesTheAgentStore(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)
	f.writeTranscript("p/b.jsonl", line1+line2)

	agentDir := filepath.Join(f.home, ".claude")
	before := snapshot(t, agentDir)

	f.run()
	f.run() // twice, so an idempotent second pass is covered too

	after := snapshot(t, agentDir)
	if len(before) != len(after) {
		t.Errorf("file count changed: %d -> %d\nbefore %v\nafter %v",
			len(before), len(after), slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
	for path, hash := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared", path)
			continue
		}
		if got != hash {
			t.Errorf("%s was modified", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was created inside the agent store", path)
		}
	}
}

// --- preview ----------------------------------------------------------------

// preview computes everything that would leave the machine and does neither upload nor commit.
func TestPreviewUploadsNothingAndCommitsNothing(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)

	o := f.opts()
	o.DryRun = true
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Shipped != 1 {
		t.Errorf("preview should report what would ship: %+v", rep)
	}
	if len(f.port.keys()) != 0 {
		t.Error("preview uploaded something")
	}
	if f.store.Len() != 0 {
		t.Error("preview committed state")
	}

	// The plan must carry the real object key and redaction figures, or it previews nothing.
	file := rep.Sources[0].Files[0]
	if file.ObjectKey == "" {
		t.Error("preview should report the object key that would be used")
	}
	if file.BytesOut == 0 {
		t.Error("preview should report the sealed size")
	}
}

// --- bounds and ordering ----------------------------------------------------

// A bounded run must report that it was bounded: silent truncation reads as "everything is
// collected". Progress must stream every decided file while the run is still going.
func TestProgressStreamsEveryDecidedFile(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)
	f.writeTranscript("p/b.jsonl", line1)

	type event struct {
		done, total int
		decision    auditlog.Decision
	}
	var events []event
	o := f.opts()
	o.Progress = func(sourceID string, done, total int, fo engine.FileOutcome) {
		if sourceID != "claude-code-transcripts" {
			t.Errorf("progress reported source %q", sourceID)
		}
		events = append(events, event{done, total, fo.Decision})
	}
	if _, err := engine.Run(context.Background(), f.store, o); err != nil {
		t.Fatal(err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 progress events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.done != i+1 || ev.total != 2 {
			t.Errorf("event %d: counter [%d/%d], want [%d/2]", i, ev.done, ev.total, i+1)
		}
		if ev.decision != auditlog.DecisionShipped {
			t.Errorf("event %d: decision %q, want shipped", i, ev.decision)
		}
	}

	// Unchanged files stream too, or the counter could never reach its total.
	events = nil
	if _, err := engine.Run(context.Background(), f.store, o); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("steady-state run: expected 2 progress events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.decision != auditlog.DecisionUnchanged {
			t.Errorf("steady-state event %d: decision %q, want unchanged", i, ev.decision)
		}
	}
}

func TestMaxFilesPerRunIsReportedNotSilent(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"p/a.jsonl", "p/b.jsonl", "p/c.jsonl", "p/d.jsonl"} {
		f.writeTranscript(p, line1)
	}
	f.eff.MaxFilesPerRun = 2

	rep := f.run()
	if rep.Shipped != 2 {
		t.Errorf("expected the run to stop at 2, got %d", rep.Shipped)
	}
	if !rep.Truncated {
		t.Error("a truncated run must say so")
	}

	// The rest arrive on the next tick: a bound is a catch-up, not a loss.
	rep2 := f.run()
	if rep2.Shipped != 2 {
		t.Errorf("the remaining files should ship next tick, got %d", rep2.Shipped)
	}
	if len(f.port.keys()) != 4 {
		t.Errorf("expected 4 objects, got %d", len(f.port.keys()))
	}
}

// A spec change resets exactly that source's state, so the file re-ships onto its existing key.
func TestSpecChangeResetsOnlyThatSource(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	f.run()
	keyBefore := f.port.keys()[0]

	f.eff.Sources[0].SpecFingerprint = strings.Repeat("b", 64)
	rep := f.run()

	if rep.Shipped != 1 {
		t.Errorf("a spec change should re-ship the source: %+v", rep)
	}
	if got := f.port.keys(); len(got) != 1 || got[0] != keyBefore {
		t.Errorf("the key must not change with the spec: %v", got)
	}
}

// After a spec change preview must report the same would-ship as a real sync, persisting nothing.
func TestPreviewReportsWouldShipAfterSpecChange(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	f.run()
	uploadsBefore := len(f.port.keys())

	f.eff.Sources[0].SpecFingerprint = strings.Repeat("b", 64)
	rep := f.runDry()

	if rep.Shipped != 1 {
		t.Errorf("preview after a spec change should report would-ship, not unchanged: %+v", rep)
	}
	if len(f.port.keys()) != uploadsBefore {
		t.Error("preview uploaded something")
	}
	if f.store.Len() != 1 {
		t.Errorf("preview must not drop entries: %d left", f.store.Len())
	}

	// A real run afterwards still re-ships, so preview changed nothing about the next sync.
	if rep := f.run(); rep.Shipped != 1 {
		t.Errorf("the real sync after preview should still re-ship: %+v", rep)
	}
}

// --- health -----------------------------------------------------------------

func TestHealthIsReportedPerSource(t *testing.T) {
	f := newFixture(t)
	// Root exists, nothing matches.
	if err := os.MkdirAll(filepath.Join(f.home, ".claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}

	rep := f.run()
	if len(rep.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(rep.Sources))
	}
	if rep.Sources[0].Health != sources.RootPresentNoMatch {
		t.Errorf("health %q, want root_present_no_match", rep.Sources[0].Health)
	}
	if rep.Sources[0].Reason == "" {
		t.Error("a non-collected health state must carry a reason")
	}
}

func TestDisabledSourceIsNotCollected(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	f.eff.Sources[0].Enabled = false

	rep := f.run()
	if rep.Shipped != 0 || len(f.port.keys()) != 0 {
		t.Errorf("a disabled source must not be collected: %+v", rep)
	}
}

func TestRunRefusesWithoutRecipients(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Recipients = nil
	if _, err := engine.Run(context.Background(), f.store, o); err == nil {
		t.Fatal("a run with no recipients must be refused: encryption is not optional")
	}
}

// --- helpers ----------------------------------------------------------------

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		rel, _ := filepath.Rel(root, path)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An expired config keeps collecting, and every object it produces says so. Stamped per object
// rather than per run, because the archive outlives the run that produced it.
func TestExpiredConfigStillShipsAndStampsEveryManifest(t *testing.T) {
	f := newFixture(t)
	f.eff.ConfigExpired = true
	f.writeTranscript("p/s1.jsonl", line1)
	f.writeTranscript("p/s2.jsonl", line2)

	rep := f.run()
	if rep.Shipped != 2 {
		t.Fatalf("an expired config stopped collection: %+v", rep)
	}
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if !m.ConfigExpired {
			t.Errorf("%s: manifest does not carry config_expired", k)
		}
	}
}

// The other direction, so the stamp is not simply always set.
func TestCurrentConfigLeavesTheStampOff(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	f.run()

	obj, _ := f.port.get(f.port.keys()[0])
	m, _, err := transforms.Open(obj.Body, f.unit.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if m.ConfigExpired {
		t.Error("config_expired is set on a run with a current config")
	}
}

// Every object an install writes must sit under ONE install root: mirror keys and the heartbeat
// once disagreed about the organization, and erasure is one prefix sweep that cannot cover two.
func TestEveryKeySitsUnderOneInstallRoot(t *testing.T) {
	f := newFixture(t)
	f.eff.OrganizationID = "acme"
	f.writeTranscript("p/s1.jsonl", line1)

	if rep := f.run(); rep.Shipped == 0 {
		t.Fatalf("nothing shipped: %+v", rep)
	}

	root, err := formats.InstallRoot(f.eff.OrganizationID, f.unit.InstallID.String())
	if err != nil {
		t.Fatal(err)
	}
	keys := f.port.keys()
	if len(keys) == 0 {
		t.Fatal("no keys written")
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, root+"/") {
			t.Errorf("key outside this install's root %s:\n  %s", root, k)
		}
	}
}

// --- M8: pause state and drain ---------------------------------------------

// A paused install collects nothing, ships nothing, and commits nothing. Checked at the engine
// rather than per verb: a per-verb check would be one new verb away from a hole.
func TestAPausedInstallShipsNothingAndCommitsNothing(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	if err := platform.Set(f.stateDir, "off to a client site", engineFixedTime(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	rep := f.run()
	if !rep.Paused {
		t.Error("the report does not say paused; a zero summary reads as nothing to collect")
	}
	if rep.PauseReason == "" {
		t.Error("the reason did not reach the report")
	}
	if rep.Shipped != 0 || len(f.port.keys()) != 0 {
		t.Fatalf("a paused install shipped: %+v %v", rep, f.port.keys())
	}

	// Nothing committed either, or resuming would treat never-shipped files as already sent.
	f.reopen()
	doc, err := engine.Peek(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 0 {
		t.Fatalf("a paused run committed %d fingerprints; resuming would skip those files",
			len(doc.Entries))
	}
}

func TestResumingCollectsTheBacklogThePauseHeldBack(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	if err := platform.Set(f.stateDir, "", engineFixedTime(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.run()

	if err := platform.Clear(f.stateDir); err != nil {
		t.Fatal(err)
	}
	rep := f.run()
	if rep.Paused {
		t.Fatal("still paused after Clear")
	}
	if rep.Shipped == 0 {
		t.Fatal("resuming shipped nothing: the paused run must not have consumed the backlog")
	}
}

// preview is exempt: it uploads and commits nothing, and a paused owner may still look.
func TestPreviewStillWorksWhilePaused(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	if err := platform.Set(f.stateDir, "", engineFixedTime(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	rep := f.runDry()
	if rep.Paused {
		t.Error("preview reported itself paused")
	}
	if rep.Shipped == 0 {
		t.Error("preview found nothing to show while paused")
	}
	if len(f.port.keys()) != 0 {
		t.Errorf("preview uploaded %v", f.port.keys())
	}
}

// The drain ignores max_files_per_run: it runs when the host is about to disappear, and with no
// spool a backlog left behind is data loss rather than a catch-up next tick.
func TestDrainIgnoresTheMaxFilesPerRunBound(t *testing.T) {
	f := newFixture(t)
	f.eff.MaxFilesPerRun = 2
	for i := 0; i < 7; i++ {
		f.writeTranscript(fmt.Sprintf("p/s%d.jsonl", i), line1)
	}

	bounded := f.run()
	if !bounded.Truncated {
		t.Fatalf("a bound of 2 did not truncate 7 files: %+v", bounded)
	}
	if bounded.Shipped > 2 {
		t.Fatalf("the bound was not applied: shipped %d", bounded.Shipped)
	}

	f.reopen()
	drained := f.runUnbounded()
	if drained.Truncated {
		t.Error("the drain reported itself truncated: the bound still applied")
	}
	// Everything that was left, in one pass.
	if drained.Shipped < 5 {
		t.Errorf("the drain shipped %d of the remaining files", drained.Shipped)
	}
	if drained.Unchanged == 0 {
		t.Error("the already-shipped files were re-shipped rather than recognised as unchanged")
	}
}

func TestADrainOnAPausedInstallStillShipsNothing(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	if err := platform.Set(f.stateDir, "", engineFixedTime(), time.Time{}); err != nil {
		t.Fatal(err)
	}

	// The drain gets no exception: switching collection off is not consent to a last upload.
	rep := f.runUnbounded()
	if !rep.Paused || rep.Shipped != 0 || len(f.port.keys()) != 0 {
		t.Fatalf("a drain overrode the pause: %+v %v", rep, f.port.keys())
	}
}

// planOf mirrors the app layer's planFor, over a realistic resolved configuration.
func planOf(eff *config.Effective) engine.Plan {
	return engine.Plan{
		OrganizationID: eff.OrganizationID,
		StateDir:       eff.StateDir,
		MaxFilesPerRun: eff.MaxFilesPerRun,
		Sources:        eff.Sources,
		RulePacks:      eff.RulePacks,
		StructuralEx:   eff.StructuralEx,
		Deny:           eff.Deny,
		ConfigVersion:  eff.ConfigVersion,
		ConfigExpired:  eff.ConfigExpired,
	}
}

// Object metadata must carry the integrity hash, not an empty string: Seal takes the manifest by
// VALUE, so the ShippedHash it computes never reaches the engine that writes the metadata.
func TestObjectMetadataCarriesTheIntegrityHash(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/s1.jsonl", line1)
	f.run()

	if len(f.port.keys()) == 0 {
		t.Fatal("nothing shipped")
	}
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		got := obj.Metadata["shipped-hash"]
		if got == "" {
			t.Errorf("%s: shipped-hash is empty in object metadata", k)
			continue
		}
		// It must describe the payload actually sealed, which the manifest inside states too.
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if got != m.ShippedHash {
			t.Errorf("%s: metadata shipped-hash %s disagrees with the sealed manifest's %s",
				k, got, m.ShippedHash)
		}
	}
}

// What batching must show is how often the state document is replaced. The count is asserted
// rather than the timing, which a fast disk would hide.
func TestARunReplacesTheStateDocumentOncePerBatchNotOncePerFile(t *testing.T) {
	f := newFixture(t)
	const files = 25
	for i := 0; i < files; i++ {
		f.writeTranscript(fmt.Sprintf("p/a%02d.jsonl", i), line1)
	}

	writes := f.countStateWrites(func() {
		if rep := f.runWith(func(o *engine.Options) { o.CommitBatch = 10 }); rep.Shipped != files {
			t.Fatalf("expected %d shipped, got %+v", files, rep)
		}
	})

	// 25 files at a batch of 10: two batches, the source-boundary flush, plus project-map's own.
	if writes > 6 {
		t.Errorf("state document replaced %d times for %d files; batching is not working", writes, files)
	}
	if writes == 0 {
		t.Error("the state document was never written; nothing was made durable")
	}
}

// Everything a run shipped must be durable when the run returns, whatever exit it takes.
func TestEverythingShippedIsDurableWhenTheRunReturns(t *testing.T) {
	f := newFixture(t)
	const files = 12
	for i := 0; i < files; i++ {
		f.writeTranscript(fmt.Sprintf("p/b%02d.jsonl", i), line1)
	}
	if rep := f.runWith(func(o *engine.Options) { o.CommitBatch = 1000 }); rep.Shipped != files {
		t.Fatalf("expected %d shipped, got %+v", files, rep)
	}

	f.reopen()
	if rep := f.run(); rep.Shipped != 0 || rep.Unchanged != files {
		t.Errorf("after a reload the run should ship nothing and see %d unchanged, got %+v",
			files, rep)
	}
}

// An unreadable file must stop consuming the run budget.
func TestAnUnreadableFileBacksOffInsteadOfBurningTheBudgetForever(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/good.jsonl", line1)
	bad := filepath.Join(f.home, ".claude", "projects", "p", "bad.jsonl")
	f.writeTranscript("p/bad.jsonl", line1)
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("this user can read a mode-000 file")
	}

	first := f.run()
	// Parked, not failed: the entry is held off with a backoff, which the fingerprint records.
	if first.Parked != 1 || first.Failed != 0 {
		t.Fatalf("expected the unreadable file to park once, got %+v", first)
	}

	// Second tick, immediately: inside the backoff the file is skipped rather than read again.
	f.reopen()
	second := f.run()
	if second.Parked != 0 {
		t.Errorf("the unreadable file was read again inside its backoff: %+v", second)
	}
	if second.Skipped != 1 {
		t.Errorf("expected it to be skipped while parked, got %+v", second)
	}
}

// The backoff must expire: there is no attempt limit, because giving up silently loses data.
func TestABackedOffFileIsRetriedOnceTheDelayPasses(t *testing.T) {
	f := newFixture(t)
	// A readable file beside it, or the shape sniff condemns the whole source instead.
	f.writeTranscript("p/good.jsonl", line1)
	bad := filepath.Join(f.home, ".claude", "projects", "p", "bad.jsonl")
	f.writeTranscript("p/bad.jsonl", line1)
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("this user can read a mode-000 file")
	}

	if rep := f.run(); rep.Parked != 1 {
		t.Fatalf("expected one parked entry, got %+v", rep)
	}
	if err := os.Chmod(bad, 0o600); err != nil { // whatever was wrong is now fixed
		t.Fatal(err)
	}

	// Two hours later: past the one-hour cap on the backoff.
	f.reopen()
	rep := f.runWith(func(o *engine.Options) {
		later := o.Now().Add(2 * time.Hour)
		o.Now = func() time.Time { return later }
	})
	if rep.Shipped != 1 {
		t.Errorf("a file that became readable was not collected after its backoff: %+v", rep)
	}
}

// A park that has been overtaken by events must clear. Otherwise `status` and `doctor` report a
// healthy file as parked for good, while the heartbeat calls the same file unchanged.
func TestAParkedFileStopsBeingParkedOnceItReadsCleanAgain(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/good.jsonl", line1)
	bad := filepath.Join(f.home, ".claude", "projects", "p", "bad.jsonl")
	f.writeTranscript("p/bad.jsonl", line1)

	// Ship it first, so the entry carries the source hash only a completed ship can write.
	if rep := f.run(); rep.Shipped != 2 {
		t.Fatalf("expected both files to ship, got %+v", rep)
	}

	// It changes, so the size-and-mtime pre-filter no longer short-circuits, and it cannot be
	// read: that is what parks it.
	f.writeTranscript("p/bad.jsonl", line1+line2)
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("this user can read a mode-000 file")
	}
	f.reopen()
	if rep := f.run(); rep.Parked != 1 {
		t.Fatalf("expected the unreadable file to park, got %+v", rep)
	}

	// Readable again, and reverted to the bytes that shipped: the next run past the backoff reads
	// it, matches the committed hash, and ships nothing. The entry must come out clean.
	if err := os.Chmod(bad, 0o600); err != nil {
		t.Fatal(err)
	}
	f.writeTranscript("p/bad.jsonl", line1)
	f.reopen()
	rep := f.runWith(func(o *engine.Options) {
		later := o.Now().Add(2 * time.Hour)
		o.Now = func() time.Time { return later }
	})
	if rep.Shipped != 0 || rep.Unchanged != 2 {
		t.Fatalf("unchanged bytes should re-ship nothing, got %+v", rep)
	}

	doc, err := engine.Peek(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := doc.Entries[engine.Key{SourceID: "claude-code-transcripts", NativePath: bad}]
	if !ok {
		t.Fatalf("no entry for %s in %+v", bad, doc.Entries)
	}
	if fp.Parked || fp.LastError != "" || fp.Attempts != 0 || !fp.BackoffUntil.IsZero() {
		t.Errorf("the park survived a clean read: %+v", fp)
	}
}

// A revoked install must fail fast: without the latch every remaining file repeats the same
// refusal and writes a park record, burying the one line that says access was revoked.
func TestARefusedInstallStopsTheRunAtTheFirstFile(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.writeTranscript(fmt.Sprintf("p/a%02d.jsonl", i), line1)
	}
	f.port.FailAll = fmt.Errorf("creds: vend failed: %w", formats.ErrCredentialsRefused)

	rep, err := engine.Run(context.Background(), f.store, f.opts())

	if !errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatalf("want a refusal error from the run, got %v", err)
	}
	if rep.Failed > 1 {
		t.Errorf("%d files failed; the run should stop at the first refusal", rep.Failed)
	}
	if rep.Shipped != 0 {
		t.Errorf("%d files shipped despite refused credentials", rep.Shipped)
	}
}

// An ordinary upload error is NOT fatal: those are per-object and the run continues.
func TestAnOrdinaryUploadErrorDoesNotStopTheRun(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.writeTranscript(fmt.Sprintf("p/b%02d.jsonl", i), line1)
	}
	f.port.FailAll = errors.New("connection reset by peer")

	rep, err := engine.Run(context.Background(), f.store, f.opts())
	if err != nil {
		t.Fatalf("an ordinary upload failure ended the run: %v", err)
	}
	if rep.Failed != 5 {
		t.Errorf("want all 5 attempted and failed, got %+v", rep)
	}
}

// The flag has to survive Run's own report construction: it was first set before the line that
// replaces the whole struct, so the discard was detected and then silently dropped on the floor.
func TestARunReportsThatItDiscardedTheStore(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/a.jsonl", line1)
	if rep := f.run(); rep.StoreCorrupt {
		t.Fatalf("a healthy store reported as corrupt: %+v", rep)
	}

	// A flipped hex digit: still valid JSON, still schema-clean, only the checksum catches it.
	path := filepath.Join(f.stateDir, engine.FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"source_hash": "`, `"source_hash": "f`, 1)
	if err := os.WriteFile(path, []byte(tampered[:len(tampered)-1]), 0o600); err != nil {
		t.Fatal(err)
	}

	f.reopen()
	if rep := f.run(); !rep.StoreCorrupt {
		t.Errorf("the run did not report discarding the store: %+v", rep)
	}
}
