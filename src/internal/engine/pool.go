package engine

import (
	"context"
	"fmt"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// One source's file pass: goroutines compute, the loop thread decides and commits. Two slot pools,
// because compute is core-bound while uploads are latency-bound, and a sealed object leaves its
// compute slot to join an authorization group. Everything shared stays on the loop goroutine, and
// the fingerprint is read there BEFORE dispatch: a source yielding a path twice would break that.

// fileJob is one candidate, complete.
type fileJob struct {
	idx  int
	cand sources.Candidate
	fp   Fingerprint
	seen bool
}

// fileResult is what comes back: what was decided, and what the loop thread must make
// durable. Neither leg commits anything, because the store has exactly one writer.
type fileResult struct {
	idx     int
	outcome FileOutcome
	intent  intent

	// unit is the raw bytes staged for the enricher, carried out through every later exit.
	unit *transforms.RawUnit

	// bytes is the candidate's size, released back to the in-flight gate when folded.
	bytes int64

	// pending lives only between the compute leg and the loop thread that stages it.
	pending *pendingPut

	// unavailable stops this run's uploads without outcome.Fatal's permanent-kill meaning.
	unavailable bool
}

// intent is the durable write a file's outcome asks for; only the loop thread applies it.
type intent struct {
	kind   intentKind
	key    Key
	fp     Fingerprint
	reason string // backoff reason
}

type intentKind int

const (
	intentNone    intentKind = iota
	intentRefresh            // unchanged content: refresh size/mtime
	intentShipped            // the only durable step, after a verified PUT
	intentBackoff            // a read or scrub failure holds the file off
)

// sourcePass is one source's file pass. Every field belongs to the loop goroutine.
type sourcePass struct {
	o     Options
	store *commitBuffer
	src   sources.Resolved
	disc  sources.Discovery
	rep   *Report
	out   *SourceOutcome

	// budget is the run-wide max_files_per_run remainder: reserved at admission, refunded in fold.
	budget *int

	// scrubber is shared by every goroutine in the pass and is immutable after transforms.New.
	scrubber *transforms.Scrubber
	scrubErr error
	staging  bool

	// Index-addressed, so the report and enricher input keep candidate order however work finishes.
	slots  []FileOutcome
	filled []bool
	units  []*transforms.RawUnit

	decided       int // what Progress reports as done
	inFlightBytes int64
	fatal         bool // the first refusal has landed; admit nothing more
	fatalReason   string

	// staged is the authorization accumulator; it belongs to the loop goroutine alone.
	staged *batcher[stagedUpload]

	// uploadHalted is fatal's non-permanent twin: this run sends and commits nothing more.
	uploadHalted bool
	haltReason   string

	// unchangedElided counts unchanged decisions not written per-file, for the one aggregate entry.
	unchangedElided int
}

// concurrency is how many files are read, scrubbed and sealed at once: GOMAXPROCS, because the
// expensive steps are pure CPU and single-threaded per file. Not configuration; Workers is a pin.
func (o Options) concurrency(candidates int) int {
	w := o.Workers
	if w <= 0 {
		w = runtime.GOMAXPROCS(0)
	}
	return max(1, min(w, candidates))
}

// uploadConcurrency is how many PUTs ride the network at once: 8x compute, because an upload holds
// no core and the extra width overlaps a group's PUTs with the next group's sealing.
func (o Options) uploadConcurrency(compute int) int {
	if o.UploadWorkers > 0 {
		return o.UploadWorkers
	}
	return 8 * compute
}

func (p *sourcePass) run(ctx context.Context) error {
	n := len(p.disc.Candidates)
	computeLimit := p.o.concurrency(n)
	uploadLimit := p.o.uploadConcurrency(computeLimit)
	// A file in flight holds exactly one slot at a time, so this many can exist at once.
	limit := computeLimit + uploadLimit
	p.slots = make([]FileOutcome, n)
	p.filled = make([]bool, n)
	p.units = make([]*transforms.RawUnit, n)

	// Buffered to the in-flight limit, so a goroutine never blocks handing a result back.
	results := make(chan fileResult, limit)
	// A group can never hold more than the in-flight limit, so a batch goroutine never blocks.
	batches := make(chan []fileResult, limit)
	computeSlots := make(chan struct{}, computeLimit)
	uploadSlots := make(chan struct{}, uploadLimit)

	// Half the upload budget bounds a group below uploadLimit, so seal and PUT overlap.
	p.staged = &batcher[stagedUpload]{
		maxObjects: max(1, min(maxBatchObjects, uploadLimit/2)),
		send: func(items []stagedUpload) {
			// One authorization, then one PUT per member. The group holds ONE upload slot for its
			// whole life; the port bounds the fan-out inside.
			go func() {
				uploadSlots <- struct{}{}
				done := p.o.sendBatch(ctx, items)
				<-uploadSlots
				batches <- done
			}()
		},
	}

	next, inFlight, computing := 0, 0, 0
	var stopped error
	for {
		for inFlight < limit && p.canAdmit(ctx, next, inFlight, &stopped) {
			job := fileJob{idx: next, cand: p.disc.Candidates[next]}
			job.fp, job.seen = p.store.Get(Key{
				SourceID:   p.src.ID,
				NativePath: job.cand.Path,
			})
			p.inFlightBytes += job.cand.Size
			go func() {
				computeSlots <- struct{}{}
				r, pending := p.o.prepareFile(job, p.src, p.disc, p.scrubber, p.scrubErr, p.staging)
				<-computeSlots
				// A sealed object goes back to the loop thread to join an authorization group.
				r.pending = pending
				results <- r
			}()
			next++
			inFlight++
			computing++
		}
		// Nothing else can join the accumulator, so send what is held rather than wait.
		if computing == 0 {
			p.staged.flush()
		}
		if inFlight == 0 {
			break
		}
		select {
		case r := <-results:
			computing--
			if r.pending != nil {
				for _, d := range p.stageUpload(r) {
					inFlight--
					p.inFlightBytes -= d.bytes
					p.fold(d)
				}
				continue
			}
			inFlight--
			p.inFlightBytes -= r.bytes
			p.fold(r)
		case done := <-batches:
			for _, r := range done {
				inFlight--
				p.inFlightBytes -= r.bytes
				p.fold(r)
			}
			// Sending the objects already accumulated would repeat one verdict per sealed sibling.
			for _, d := range p.drainStaged() {
				inFlight--
				p.inFlightBytes -= d.bytes
				p.fold(d)
			}
		}
	}

	p.assemble()

	// The aggregate for the unchanged entries fold elided, written even if a refusal ended the pass.
	if p.unchangedElided > 0 && p.o.Log != nil {
		noun := "files"
		if p.unchangedElided == 1 {
			noun = "file"
		}
		_ = p.o.Log.Append(auditlog.Entry{
			Decision:      auditlog.DecisionUnchanged,
			SourceID:      p.src.ID,
			ConfigVersion: p.o.Plan.ConfigVersion,
			Reason:        fmt.Sprintf("%d %s unchanged by size and mtime, not opened; per-file entries elided", p.unchangedElided, noun),
		})
	}

	// Stop the whole run on a refusal: it has nowhere to put anything more it collects.
	if p.fatal {
		if p.o.Log != nil {
			_ = p.o.Log.Append(auditlog.Entry{
				Decision: auditlog.DecisionFailed,
				SourceID: p.src.ID,
				Reason:   "run abandoned: " + p.fatalReason,
			})
		}
		return fmt.Errorf("%w: collection stopped after %d of %d files; "+
			"this install may have been revoked or re-enrolled elsewhere. "+
			"`shipper doctor` reports what the control plane says",
			formats.ErrCredentialsRefused, len(p.out.Files), len(p.disc.Candidates))
	}
	// Not a kill: nothing new was committed, and the next tick tries again.
	if p.uploadHalted {
		p.out.Reason = "uploads stopped: " + p.haltReason
		if p.o.Log != nil {
			_ = p.o.Log.Append(auditlog.Entry{
				Decision: auditlog.DecisionFailed,
				SourceID: p.src.ID,
				Reason:   p.out.Reason,
			})
		}
		return fmt.Errorf("%w: collection stopped after %d of %d files; "+
			"nothing new was committed and the next run retries. %s",
			ErrUploadUnavailable, len(p.out.Files), len(p.disc.Candidates), p.haltReason)
	}
	if stopped != nil {
		return stopped
	}
	// Truncation is a budget verdict only; cancellation and refusal have already returned above.
	if next < n {
		p.rep.Truncated = true
		p.out.Remaining = n - next
		p.rep.Remaining += n - next
	}
	return nil
}

// canAdmit is every gate on starting one more file, in the order they matter.
func (p *sourcePass) canAdmit(ctx context.Context, next, inFlight int, stopped *error) bool {
	if *stopped != nil || p.fatal || p.uploadHalted || next >= len(p.disc.Candidates) {
		return false
	}
	if err := ctx.Err(); err != nil {
		// The drain deadline arrives here: admit nothing more, but collect and commit what runs.
		*stopped = err
		return false
	}
	if *p.budget <= 0 {
		return false
	}
	// Memory, not concurrency: the memstat limit was derived from ONE worst-case file, so the sum
	// of raw bytes in flight is held to the same figure. A file larger than the gate runs alone.
	if sz := p.disc.Candidates[next].Size; inFlight > 0 && p.inFlightBytes+sz > platform.MaxInFlightBytes() {
		return false
	}
	// Budget is RESERVED here and refunded in fold, which is what makes overshoot impossible.
	*p.budget--
	return true
}

// fold is the only place a result touches the report, store, audit log and progress stream.
func (p *sourcePass) fold(r fileResult) {
	// Only the first refusal or unavailable verdict is counted, or rep.Failed becomes a function
	// of GOMAXPROCS. The duplicates' intents still apply: one may have shipped before the refusal.
	if (r.outcome.Fatal && p.fatal) || (r.unavailable && p.uploadHalted) {
		p.applyIntent(&r)
		return
	}

	p.applyIntent(&r)
	p.slots[r.idx], p.filled[r.idx] = r.outcome, true
	p.units[r.idx] = r.unit
	p.decided++

	switch r.outcome.Decision {
	case auditlog.DecisionShipped:
		p.rep.Shipped++
	case auditlog.DecisionUnchanged:
		p.rep.Unchanged++
		*p.budget++
	case auditlog.DecisionSkipped:
		p.rep.Skipped++
		*p.budget++
	case auditlog.DecisionParked:
		p.rep.Parked++
	case auditlog.DecisionFailed:
		p.rep.Failed++
	}

	if r.outcome.Fatal {
		// The refusal gets no progress line and no per-file entry; the pass writes one at the end.
		p.fatal = true
		p.fatalReason = r.outcome.Reason
		return
	}
	if r.unavailable {
		// Latched on the loop thread, so the admission gate and accumulator see it on the next turn.
		p.uploadHalted = true
		p.haltReason = r.outcome.Reason
		return
	}

	if p.o.Progress != nil {
		// done counts decisions, not positions: still monotonic, and still ends at total.
		p.o.Progress(p.src.ID, p.decided, len(p.disc.Candidates), r.outcome)
	}
	if p.o.Log != nil {
		// Elided into one aggregate entry: a line per unchanged file grows the log at scan rate.
		// Only files the run never OPENED are elided; reading one is doing something worth a line.
		if r.outcome.Decision == auditlog.DecisionUnchanged && r.outcome.BytesIn == 0 {
			p.unchangedElided++
			return
		}
		_ = p.o.Log.Append(auditlog.Entry{
			Decision:         r.outcome.Decision,
			SourceID:         r.outcome.SourceID,
			File:             r.outcome.NativePath,
			BytesIn:          r.outcome.BytesIn,
			BytesOut:         r.outcome.BytesOut,
			RedactionDensity: r.outcome.Density,
			RuleHits:         r.outcome.RuleHits,
			ObjectKey:        r.outcome.ObjectKey,
			ConfigVersion:    p.o.Plan.ConfigVersion,
			Reason:           r.outcome.Reason,
		})
	}
}

// applyIntent makes a result durable. A failed commit rewrites the outcome BEFORE fold counts it,
// so "shipped but the record was lost" reads as failed.
func (p *sourcePass) applyIntent(r *fileResult) {
	if r.intent.kind == intentNone {
		return
	}
	err := p.store.Commit(r.intent.key, r.intent.fp)
	if err == nil {
		return
	}
	switch r.intent.kind {
	case intentRefresh:
		r.outcome.Decision = auditlog.DecisionFailed
		r.outcome.Reason = err.Error()
	case intentShipped:
		r.outcome.Decision = auditlog.DecisionFailed
		r.outcome.Reason = "upload succeeded but commit failed: " + err.Error()
	case intentBackoff:
		r.outcome.Reason = r.intent.reason +
			" (and the backoff could not be recorded: " + err.Error() + ")"
	}
}

// stageUpload puts one sealed object into the authorization accumulator, which sends the group when
// the next object would take it past either bound. It returns results that are already final.
func (p *sourcePass) stageUpload(r fileResult) []fileResult {
	pending := r.pending
	r.pending = nil
	it := stagedUpload{res: r, pending: pending}

	if p.fatal || p.uploadHalted {
		return []fileResult{p.abandon(it)}
	}
	p.staged.add(it, int64(len(pending.obj)))
	return nil
}

// drainStaged empties the accumulator once the run has stopped uploading.
func (p *sourcePass) drainStaged() []fileResult {
	if p.staged.len() == 0 || (!p.fatal && !p.uploadHalted) {
		return nil
	}
	items := p.staged.take()
	out := make([]fileResult, len(items))
	for i, it := range items {
		out[i] = p.abandon(it)
	}
	return out
}

// assemble moves the slots into the source outcome in candidate order; unfilled ones never decided.
func (p *sourcePass) assemble() {
	for i, ok := range p.filled {
		if ok {
			p.out.Files = append(p.out.Files, p.slots[i])
		}
	}
}

// stagedUnits is the enricher's input, in candidate order like everything else.
func (p *sourcePass) stagedUnits() []transforms.RawUnit {
	var staged []transforms.RawUnit
	for _, u := range p.units {
		if u != nil {
			staged = append(staged, *u)
		}
	}
	return staged
}
