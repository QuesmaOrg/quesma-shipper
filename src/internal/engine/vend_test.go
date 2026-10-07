package engine_test

// The upload path: bounded authorization groups, unconditional PUTs, per-object commits. Every
// test here runs the real loop against fakePort, which stands in for the whole network leg.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
)

// vendRun runs the loop with the port wired and returns the report and the run's error.

func vendRun(f *fixture, port *fakePort, adjust func(*engine.Options)) (engine.Report, error) {
	o := f.opts()
	o.Upload = port
	if adjust != nil {
		adjust(&o)
	}
	return engine.Run(context.Background(), f.store, o)
}

// One authorization for a small run, one PUT per object, one fingerprint per PUT.

func TestTheUploadPathShipsOneGroupForASmallRun(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.writeTranscript(fmt.Sprintf("p/v%02d.jsonl", i), line1)
	}
	port := newPort()

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 4 })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if rep.Shipped != 5 {
		t.Errorf("shipped %d of 5: %+v", rep.Shipped, rep)
	}
	if got := port.sizes(); len(got) != 1 || got[0] != 5 {
		t.Errorf("groups %v; five files fit in one authorization", got)
	}
	port.storedOnce(t)
	for _, fo := range rep.Sources[0].Files {
		if _, ok := f.store.Get(engine.Key{SourceID: fo.SourceID, ID: fo.NativePath}); !ok {
			t.Fatalf("%s committed no fingerprint", fo.RelPath)
		}
	}
}

// The group is bounded by object count, and the remainder must not wait for a full group.

func TestAnOversizedRunSplitsIntoBoundedGroups(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 40; i++ {
		f.writeTranscript(fmt.Sprintf("p/g%02d.jsonl", i), line1)
	}
	port := newPort()

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 8 })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if rep.Shipped != 40 {
		t.Errorf("shipped %d of 40: %+v", rep.Shipped, rep)
	}
	sizes := port.sizes()
	if len(sizes) < 2 {
		t.Fatalf("40 files were authorized in %d group(s); the bound is 32", len(sizes))
	}
	total := 0
	for _, n := range sizes {
		if n > 32 {
			t.Errorf("a group carried %d objects, over the 32 bound; groups were %v", n, sizes)
		}
		total += n
	}
	if total != 40 {
		t.Errorf("groups carried %d objects for 40 files: %v", total, sizes)
	}
	port.storedOnce(t)
}

// One object's failure is its own. Its siblings commit, and the next run re-prepares only it.

func TestAFailedObjectDoesNotDiscardItsSiblings(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/bad.jsonl", line1)
	for i := 0; i < 4; i++ {
		f.writeTranscript(fmt.Sprintf("p/ok%02d.jsonl", i), line1)
	}
	port := newPort()
	// Whichever file the first group starts with: keys are keyed hashes, not pickable by name.
	var failed string
	port.verdict = func(call, idx int, o engine.PreparedObject) error {
		if call == 0 && idx == 0 {
			failed = o.Key
			return errors.New("upload: object store answered HTTP 500")
		}
		return nil
	}

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 4 })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Shipped != 4 || rep.Failed != 1 {
		t.Errorf("want 4 shipped and 1 failed, got %d and %d", rep.Shipped, rep.Failed)
	}
	if rep.Parked != 0 {
		t.Errorf("%d objects parked; an unconditional PUT has no precondition to park on", rep.Parked)
	}

	// The failure persisted nothing, so the second run re-prepares exactly that one file.
	healthy := newPort()
	rep2, err := vendRun(f, healthy, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Shipped != 1 || rep2.Unchanged != 4 {
		t.Errorf("second run shipped %d and left %d unchanged, want 1 and 4", rep2.Shipped, rep2.Unchanged)
	}
	if got := healthy.groups; len(got) != 1 || len(got[0]) != 1 || got[0][0] != failed {
		t.Errorf("second run authorized %v; only the failed object had anything to send", got)
	}
}

// A refused install is the kill path: the run stops and the duplicates in flight are one fact.

func TestARefusedAuthorizationStopsTheRun(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.writeTranscript(fmt.Sprintf("p/r%02d.jsonl", i), line1)
	}
	port := newPort()
	port.verdict = func(int, int, engine.PreparedObject) error {
		return fmt.Errorf("backend: refused: %w", formats.ErrCredentialsRefused)
	}
	beat := 0

	rep, err := vendRun(f, port, func(o *engine.Options) {
		o.Workers = 4
		o.Heartbeat = func(context.Context, engine.Report) error { beat++; return nil }
	})

	if !errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatalf("want a refusal from the run, got %v", err)
	}
	if rep.Failed != 1 {
		t.Errorf("%d refusals counted; duplicates are the same fact about the same install", rep.Failed)
	}
	if beat != 0 {
		t.Errorf("the heartbeat ran %d times after a refusal", beat)
	}
	if rep.Shipped != 0 {
		t.Errorf("%d objects shipped against a refused install", rep.Shipped)
	}
	if got := port.putCount(); got != 0 {
		t.Errorf("%d objects stored against a revoked install", got)
	}
}

// An unavailable control plane is NOT a kill: nothing new commits, and the next run ships it.

func TestAnUnavailableControlPlaneStopsUploadsWithoutKillingTheInstall(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 12; i++ {
		f.writeTranscript(fmt.Sprintf("p/u%02d.jsonl", i), line1)
	}
	port := newPort()
	port.verdict = func(int, int, engine.PreparedObject) error {
		return fmt.Errorf("backend: HTTP 409: %w", engine.ErrUploadUnavailable)
	}

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 4 })

	if !errors.Is(err, engine.ErrUploadUnavailable) {
		t.Fatalf("want an unavailable-authorization error, got %v", err)
	}
	if errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatal("an unavailable control plane wrapped ErrCredentialsRefused; that kills the install")
	}
	if rep.Shipped != 0 {
		t.Errorf("%d objects shipped while authorization was unavailable", rep.Shipped)
	}
	if rep.Failed != 1 {
		t.Errorf("%d failures counted; one refused group is one fact about the control plane", rep.Failed)
	}

	healthy := newPort()
	rep2, err := vendRun(f, healthy, func(o *engine.Options) { o.Workers = 4 })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Shipped != 12 {
		t.Errorf("second run shipped %d of the 12 held back: %+v", rep2.Shipped, rep2)
	}
	healthy.storedOnce(t)
}

// The same fact across several groups: objects sealed when the halt lands are drained, and

// counting each would make the failure count a function of what was in flight.

func TestAnUnavailableControlPlaneCountsOnceAcrossManyGroups(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 40; i++ {
		f.writeTranscript(fmt.Sprintf("p/m%02d.jsonl", i), line1)
	}
	port := newPort()
	port.verdict = func(int, int, engine.PreparedObject) error {
		return fmt.Errorf("backend: HTTP 503: %w", engine.ErrUploadUnavailable)
	}

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 8 })

	if !errors.Is(err, engine.ErrUploadUnavailable) {
		t.Fatalf("want an unavailable-authorization error, got %v", err)
	}
	if rep.Failed != 1 {
		t.Errorf("%d failures counted over 40 files; one halted run is one fact", rep.Failed)
	}
	if rep.Shipped != 0 {
		t.Errorf("%d objects shipped while authorization was unavailable", rep.Shipped)
	}
	if f.store.Len() != 0 {
		t.Errorf("%d fingerprints committed by a run that sent nothing", f.store.Len())
	}
}

// An expired ticket earns exactly one more authorization, and the object ships on it.

func TestAnExpiredTicketIsReauthorizedOnce(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/e0.jsonl", line1)
	port := newPort()
	port.verdict = func(call, _ int, _ engine.PreparedObject) error {
		if call == 0 {
			return fmt.Errorf("upload: HTTP 403: %w", engine.ErrTicketExpired)
		}
		return nil
	}

	rep, err := vendRun(f, port, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Shipped != 1 {
		t.Errorf("the reauthorized object did not ship: %+v", rep)
	}
	if got := port.sizes(); len(got) != 2 {
		t.Errorf("%d authorizations for one expiry, want 2: %v", len(got), got)
	}
	port.storedOnce(t)
}

// The reauthorization is bounded to ONE: a second expiry is a wrong clock or a wrong lease.

func TestReauthorizationDoesNotLoop(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/e1.jsonl", line1)
	port := newPort()
	port.verdict = func(int, int, engine.PreparedObject) error {
		return fmt.Errorf("upload: HTTP 403: %w", engine.ErrTicketExpired)
	}

	rep, err := vendRun(f, port, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Failed != 1 || rep.Shipped != 0 {
		t.Errorf("want the object failed and nothing shipped, got %d failed and %d shipped",
			rep.Failed, rep.Shipped)
	}
	if got := len(port.sizes()); got != 2 {
		t.Errorf("%d authorizations for a permanently expired ticket, want exactly 2", got)
	}
	if _, ok := f.store.Get(engine.Key{SourceID: "claude-code-transcripts",
		ID: f.home + "/.claude/projects/p/e1.jsonl"}); ok {
		t.Error("a ticket that never uploaded committed a fingerprint")
	}
}

// What the race detector is here for: overlapping compute and groups, one PUT and one commit per

// key, with the accumulator on the loop thread alone.

func TestTheUploadPathShipsEachKeyExactlyOnceUnderRace(t *testing.T) {
	f := newFixture(t)
	const files = 96
	for i := 0; i < files; i++ {
		f.writeTranscript(fmt.Sprintf("p/x%03d.jsonl", i), line1)
	}
	f.eff.MaxFilesPerRun = files
	port := newPort()

	rep, err := vendRun(f, port, func(o *engine.Options) {
		o.Plan = planOf(f.eff)
		o.Workers = 8
		o.UploadWorkers = 3
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Shipped != files {
		t.Fatalf("shipped %d of %d: %+v", rep.Shipped, files, rep)
	}
	port.storedOnce(t)

	port.mu.Lock()
	distinct := len(port.objects)
	port.mu.Unlock()
	if distinct != files {
		t.Errorf("%d distinct keys stored for %d files", distinct, files)
	}
	for _, n := range port.sizes() {
		if n > 32 {
			t.Errorf("a group carried %d objects, over the 32 bound", n)
		}
	}
	if f.store.Len() != files {
		t.Errorf("%d fingerprints committed for %d shipped files", f.store.Len(), files)
	}
}

// Derived objects take the upload path too, after every raw unit of the source has shipped.

func TestDerivedObjectsShipThroughTheUploadPath(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	port := newPort()

	o := enrichOpts(t, f, db, true)
	o.Upload = port
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if rep.Shipped != 2 {
		t.Fatalf("expected a raw + derived pair, shipped %d: %+v", rep.Shipped, rep.Sources)
	}
	port.storedOnce(t)

	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.objects) != 2 {
		t.Errorf("%d distinct keys authorized for a raw + derived pair", len(port.objects))
	}
	if len(port.groups) != 2 {
		t.Errorf("groups %v; the raw pass and the enricher each authorize their own", port.groups)
	}
}

// An unauthorized enricher group reports once and commits nothing, while raw objects keep their

// commits. The halt must come back out of engine.Run, or the run would exit zero and look healthy.

func TestAnUnavailableControlPlaneStopsTheDerivedGroup(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	port := newPort()
	// The raw pass ships; only the enricher's group is refused.
	port.verdict = func(call, _ int, _ engine.PreparedObject) error {
		if call == 0 {
			return nil
		}
		return fmt.Errorf("backend: HTTP 503: %w", engine.ErrUploadUnavailable)
	}

	o := enrichOpts(t, f, db, true)
	o.Upload = port
	rep, err := engine.Run(context.Background(), f.store, o)
	if !errors.Is(err, engine.ErrUploadUnavailable) {
		t.Fatalf("the derived halt did not reach the caller: %v", err)
	}
	if errors.Is(err, formats.ErrCredentialsRefused) {
		t.Error("an unavailable control plane killed the install from the derived path")
	}
	if rep.Shipped != 1 {
		t.Errorf("shipped %d; the raw object ships and the derived one does not", rep.Shipped)
	}
	derived := 0
	for _, fo := range rep.Sources[0].Files {
		if !fo.Derived {
			continue
		}
		derived++
		if fo.Decision != formats.DecisionFailed {
			t.Errorf("the derived object decided %q while authorization was unavailable", fo.Decision)
		}
	}
	if derived != 1 {
		t.Errorf("%d derived outcomes reported, want 1", derived)
	}
}

// The halt latch belongs to the source, not to one enricher: a second enricher must not ship

// under credentials the control plane has just rejected.

func TestARefusedDerivedGroupStopsTheRemainingEnrichers(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	port := newPort()
	// Call 0 is the raw pass; the enricher's group is what gets refused.
	port.verdict = func(call, _ int, _ engine.PreparedObject) error {
		if call == 0 {
			return nil
		}
		return fmt.Errorf("backend: refused: %w", formats.ErrCredentialsRefused)
	}

	// Sorted after cursor-transcript-join, because enrichers run in id order.
	second := &countingEnricher{id: "zz-never-runs"}
	o := enrichOpts(t, f, db, true)
	o.Upload = port
	o.Enrichers = transforms.NewRegistry(&fixtureEnricher{Enricher: cursorjoin.New(), db: db}, second)
	o.Plan.Sources[0].Enrichers[second.id] = true

	if _, err := engine.Run(context.Background(), f.store, o); !errors.Is(err, formats.ErrCredentialsRefused) {
		t.Fatalf("a refused derived group did not stop the run: %v", err)
	}
	if second.calls != 0 {
		t.Errorf("the second enricher ran %d time(s) after the first was refused", second.calls)
	}
}

// countingEnricher derives nothing and records whether it was asked to.

type countingEnricher struct {
	id    string
	calls int
}

func (e *countingEnricher) ID() string { return e.id }

func (e *countingEnricher) Version() int { return 1 }

func (e *countingEnricher) Table() string { return "" }

func (e *countingEnricher) Keyspaces() []string { return nil }

func (e *countingEnricher) DBCandidates() []string { return nil }

func (e *countingEnricher) NeedsUnits() bool { return true }

func (e *countingEnricher) Enrich(transforms.Input) transforms.EnrichResult {
	e.calls++
	return transforms.EnrichResult{EnricherID: e.id, Version: 1}
}

// A control plane that cannot be reached halts the run's uploads like an unavailable one, and the

// run's error says offline so the judge can file the machine, not the shipper, as the cause.

func TestAnUnreachableControlPlaneHaltsTheRunAsOffline(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 12; i++ {
		f.writeTranscript(fmt.Sprintf("p/o%02d.jsonl", i), line1)
	}
	port := newPort()
	port.FailAll = fmt.Errorf("%w: backend: /v2/uploads/authorize: dial tcp: lookup cp.example: no such host", engine.ErrOffline)

	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 4 })
	if !errors.Is(err, engine.ErrOffline) {
		t.Fatalf("want an offline halt, got %v", err)
	}
	if errors.Is(err, formats.ErrCredentialsRefused) || errors.Is(err, engine.ErrUploadUnavailable) {
		t.Fatal("offline reads as a refusal or an unavailable control plane")
	}
	if rep.Shipped != 0 || rep.Failed != 1 {
		t.Errorf("shipped %d, failed %d; one unreachable control plane is one fact", rep.Shipped, rep.Failed)
	}
	if port.calls != 1 {
		t.Errorf("%d authorizations attempted while offline; the first verdict halts the rest", port.calls)
	}

	healthy := newPort()
	rep2, err := vendRun(f, healthy, func(o *engine.Options) { o.Workers = 4 })
	if err != nil || rep2.Shipped != 12 {
		t.Fatalf("the next run did not ship everything: shipped %d, %v", rep2.Shipped, err)
	}
}

// A PUT refused before the control plane went away is a second failure in the report, which is

// how the judge tells a run that failed and then lost the network from one that only lost it.

func TestAFailureBeforeAnOfflineHaltStaysInTheReport(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.writeTranscript(fmt.Sprintf("p/m%02d.jsonl", i), line1)
	}
	port := newPort()
	port.verdict = func(call, _ int, _ engine.PreparedObject) error {
		if call == 0 {
			return errors.New("upload: HTTP 403 AccessDenied")
		}
		return fmt.Errorf("%w: backend: dial tcp: lookup cp.example: no such host", engine.ErrOffline)
	}
	rep, err := vendRun(f, port, func(o *engine.Options) { o.Workers = 1; o.UploadWorkers = 1 })
	if !errors.Is(err, engine.ErrOffline) {
		t.Fatalf("want the offline halt, got %v", err)
	}
	if rep.Failed != 2 || rep.Shipped != 0 {
		t.Fatalf("want the refused PUT and the halt, got failed=%d shipped=%d", rep.Failed, rep.Shipped)
	}
}

type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) record(stage, source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, strings.TrimSpace(stage+" "+source))
}

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

func (l *stepLog) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range l.all() {
			if s == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stage %q never reported; saw %v", want, l.all())
}

func inOrder(steps, want []string) bool {
	at := 0
	for _, s := range steps {
		if at < len(want) && s == want[at] {
			at++
		}
	}
	return at == len(want)
}

func TestAStageReportsSealingAndUploadingWhenTheyOverlap(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.writeTranscript(fmt.Sprintf("p/v%02d.jsonl", i), line1)
	}
	port := newPort()
	release := make(chan struct{})
	port.verdict = func(call, _ int, _ engine.PreparedObject) error {
		if call == 0 {
			<-release
		}
		return nil
	}
	var log stepLog
	done := make(chan error, 1)
	go func() {
		_, err := vendRun(f, port, func(o *engine.Options) {
			o.Workers = 1
			o.Step = log.record
			o.Heartbeat = func(context.Context, engine.Report) error { return nil }
		})
		done <- err
	}()
	// The first group blocks; the loop keeps sealing behind it until every file is sealed.
	log.waitFor(t, "read and seal + upload claude-code-transcripts")
	log.waitFor(t, "upload claude-code-transcripts")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !inOrder(log.all(), []string{"discover claude-code-transcripts", "read and seal claude-code-transcripts",
		"read and seal + upload claude-code-transcripts", "upload claude-code-transcripts",
		"commit claude-code-transcripts", "heartbeat"}) {
		t.Fatalf("stages out of order: %v", log.all())
	}
}

func TestADerivedUploadIsReportedAsAnUpload(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	port := newPort()
	var log stepLog
	o := enrichOpts(t, f, db, true)
	o.Upload = port
	o.Step = log.record
	if _, err := engine.Run(context.Background(), f.store, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	steps := log.all()
	src := ""
	for _, s := range steps {
		if strings.HasPrefix(s, "enrich ") {
			src = strings.TrimPrefix(s, "enrich ")
		}
	}
	if src == "" {
		t.Fatalf("no enrich stage reported: %v", steps)
	}
	if !inOrder(steps, []string{"enrich " + src, "upload " + src, "enrich " + src}) {
		t.Fatalf("the derived PUT was not reported as an upload inside enrichment: %v", steps)
	}
}

func TestEveryDurableWriteReportsCommit(t *testing.T) {
	f := newFixture(t)
	gone := f.writeTranscript("p/w00.jsonl", line1)
	f.writeTranscript("p/w01.jsonl", line1)
	var first stepLog
	if _, err := vendRun(f, newPort(), func(o *engine.Options) { o.Step = first.record }); err != nil {
		t.Fatal(err)
	}
	// A fresh store: EnsureSpec writes the spec before any file is read.
	if !inOrder(first.all(), []string{"discover claude-code-transcripts", "commit claude-code-transcripts", "read and seal claude-code-transcripts"}) {
		t.Fatalf("the spec write during discovery did not read as commit: %v", first.all())
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	var second stepLog
	if _, err := vendRun(f, newPort(), func(o *engine.Options) { o.Step = second.record }); err != nil {
		t.Fatal(err)
	}
	// Nothing new shipped, so nothing was pending; the one write is DropVanished's deletion.
	if !inOrder(second.all(), []string{"read and seal claude-code-transcripts", "commit claude-code-transcripts"}) {
		t.Fatalf("the deletion write did not read as commit: %v", second.all())
	}
}
