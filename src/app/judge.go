package app

// The install's memory of its failures, carried by heartbeats. Persisted when the disk allows it
// and held in memory when it does not, and a failed tick ships its own failure heartbeat: without
// both, a machine that fails every tick is indistinguishable from an idle one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const (
	lastFailureFile = "last-failure.json"
	maxFailureBytes = 1 << 16
)

// JudgeTick decides whether a flush counts as a failure and persists that decision. It RETURNS the
// error the run should be judged by, which is not always the one passed in:
//
//   - lock contention becomes nil and records nothing, because another flush IS running
//   - a nil error with every attempted upload failed becomes a failure, because that is the shape a
//     real outage takes
//
// A locally-caused failure also ships a failure heartbeat, which can block up to
// failureHeartbeatTimeout. Best-effort throughout: bookkeeping must never change a run's outcome.
func (r *Runtime) JudgeTick(err error, rep formats.Report, panicked bool, mem platform.Delta) error {
	kind := formats.FailureTick
	if panicked {
		kind = formats.FailurePanic
	}
	return r.judge(err, rep, kind, mem)
}

// JudgeFinalSlice is the SIGTERM drain: the same judgement, filed under its own kind because a
// failed last slice on a host that is going away is loss rather than a retry.
func (r *Runtime) JudgeFinalSlice(err error, rep formats.Report, mem platform.Delta) error {
	return r.judge(err, rep, formats.FailureShutdown, mem)
}

func (r *Runtime) judge(err error, rep formats.Report, kind string, mem platform.Delta) error {
	if errors.Is(err, engine.ErrLocked) {
		return nil
	}
	if err == nil && rep.Shipped == 0 && rep.Failed > 0 {
		// One reason travels: the count alone cannot tell a refused PUT from an unreachable
		// control plane, and identical messages make the log unactionable.
		err = fmt.Errorf("the run shipped nothing: all %d attempted uploads failed: %s",
			rep.Failed, firstFailureReason(rep))
	}

	r.persistRecord("tick outcome", os.Stderr, func(rec *formats.FailureRecord) {
		rec.Facts = r.runFacts(rep, mem)

		// Recorded, never counted: the discard costs one re-ship rather than failing the run, but it
		// is the one condition that can otherwise lose a file for good.
		if rep.StoreCorrupt {
			rec.Append(newEvent(r.eff.StateDir, r.runID, formats.FailureStoreCorrupt,
				"the fingerprint store could not be loaded and was discarded; the next sync replaces it"))
		}

		if err == nil {
			// Persisted on a clean run too, on purpose twice over: a healthy run's cost is the
			// baseline that makes the next one's readable, and the next heartbeat is built
			// mid-flush BEFORE judging, so it can only read these facts off disk; an in-memory
			// shortcut would silently empty the field.
			rec.ConsecutiveFailures = 0
		} else {
			ev := newEvent(r.eff.StateDir, r.runID, kind, err.Error())
			rec.Append(ev)
			if ev.Counted() {
				rec.ConsecutiveFailures++
			}
		}
	})

	if err == nil {
		// Kept for the stall heartbeat, which fires mid-tick when no fresh report exists yet.
		r.lastRep = rep
	}

	// A failed run's own heartbeat never shipped (the engine sends it only on success), so without
	// this the record waits for a future healthy run that a full disk may never grant. Skipped when
	// the uploads themselves failed (another attempt could only stall the loop for one more
	// timeout) and on the SIGTERM drain, whose host is going away either way.
	if err != nil && kind != formats.FailureShutdown && rep.Failed == 0 {
		r.shipFailureHeartbeat(context.Background(), rep, "failure record", os.Stderr)
	}
	return err
}

const failureHeartbeatTimeout = 30 * time.Second

// shipFailureHeartbeat uploads the record outside the engine's own success-path heartbeat.
// mirror=false always: the local heartbeat.json keeps describing the last real flush, which is
// what doctor reads. A cancelled parent (the tick finished mid-send) is not worth a warning.
func (r *Runtime) shipFailureHeartbeat(parent context.Context, rep formats.Report, what string, errOut io.Writer) {
	if r.upload == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, failureHeartbeatTimeout)
	defer cancel()
	if err := r.writeHeartbeat(ctx, rep, false); err != nil && parent.Err() == nil {
		fmt.Fprintf(errOut, "warning: could not ship the %s: %v\n", what, err)
	}
}

// The record survives in memory even when its write does not, so readers must come through here.
func (r *Runtime) loadRecordLocked() *formats.FailureRecord {
	if r.rec == nil {
		rec := readFailureRecord(r.eff.StateDir)
		r.rec = &rec
	}
	return r.rec
}

// persistRecord applies one mutation and writes the result. The in-memory copy is kept only while
// the disk refuses the write: dropping it after a success means the next mutation re-reads the
// file, so events appended by other processes merge instead of being clobbered by a stale
// snapshot. The write runs outside the lock, so an fsync wedged on the very disk being diagnosed
// cannot block the flush's own heartbeat, which reads the record under the same mutex.
func (r *Runtime) persistRecord(what string, errOut io.Writer, mutate func(*formats.FailureRecord)) {
	r.recMu.Lock()
	rec := r.loadRecordLocked()
	mutate(rec)
	snapshot := *rec
	snapshot.Recent = append([]formats.FailureEvent(nil), rec.Recent...)
	r.recMu.Unlock()

	if err := writeFailureRecord(r.eff.StateDir, snapshot); err != nil {
		fmt.Fprintf(errOut, "warning: could not record the %s: %v\n", what, err)
		return
	}
	r.recMu.Lock()
	r.rec = nil
	r.recMu.Unlock()
}

// WatchStalledTick reports a tick that outlives its own interval, because a run stuck forever
// never reaches the judge that would say so. One standing event, refreshed on every fire, describes
// the current stall; the warning and the heartbeat repeat on doubling intervals, so a fleet-wide
// stall cannot become a fleet-wide request storm.
func (r *Runtime) WatchStalledTick(ctx context.Context, n int, every time.Duration, errOut io.Writer) {
	if every <= 0 {
		return
	}
	started := time.Now()
	for wait := every; ; wait *= 2 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		elapsed := time.Since(started).Round(time.Second)
		fmt.Fprintf(errOut, "warning: tick %d still running after %s\n", n, elapsed)
		r.persistRecord("stalled tick", errOut, func(rec *formats.FailureRecord) {
			e := newEvent(r.eff.StateDir, r.runID, formats.FailureStalled,
				fmt.Sprintf("tick %d still running after %s", n, elapsed))
			// Replaced, not skipped: clean ticks append nothing, so a stall that recovered stays the
			// newest event indefinitely, and a later stall must not hide behind its stale timestamp.
			if last := rec.Latest(); last != nil && last.Kind == formats.FailureStalled {
				*last = e
			} else {
				rec.Append(e)
			}
		})
		// The last completed tick's report rides along: this heartbeat overwrites the remote
		// object, and blanking per-source health would make a stalled install read as an idle one.
		r.shipFailureHeartbeat(ctx, r.lastRep, "stall heartbeat", errOut)
	}
}

// RecordCrash persists how the previous run died, written by the run that discovered it: the dead
// run could say nothing itself.
func RecordCrash(crash *formats.LastCrash) {
	if crash == nil {
		return
	}
	appendWithoutRuntime("crash", func(dir string, rec *formats.FailureRecord) bool {
		// One event per dead run, not per restart: an undelivered crash is re-detected on every
		// start until a heartbeat carries it out, and duplicates would evict the rest of the log.
		for i := len(rec.Recent) - 1; i >= 0; i-- {
			if rec.Recent[i].Kind == formats.FailureCrash {
				if rec.Recent[i].RunID == crash.RunID {
					return false
				}
				break
			}
		}
		// Uncounted: crashes keep their own counter, reset by delivery rather than by success.
		rec.Append(newEvent(dir, crash.RunID, formats.FailureCrash, fmt.Sprintf(
			"previous run never exited: last step %q; %d consecutive unclean run(s)", crash.Phase, crash.Consecutive)))
		return true
	})
}

// runFacts snapshots what the run cost and what it was allowed to do. Written on every outcome,
// including a clean one: "the last run was fine and here is what it cost" is what makes the run
// after it comparable.
func (r *Runtime) runFacts(rep formats.Report, mem platform.Delta) *formats.RunFacts {
	return &formats.RunFacts{
		GOMAXPROCS:       runtime.GOMAXPROCS(0),
		MaxFilesPerRun:   r.eff.MaxFilesPerRun,
		MaxInFlightBytes: platform.MaxInFlightBytes(),
		SoftLimitBytes:   platform.SoftLimit(),
		HeapInuseBytes:   mem.After.HeapInuse,
		SysBytes:         mem.After.Sys,
		GCCycles:         mem.After.NumGC - mem.Before.NumGC,

		SlowestScrubNanos: rep.SlowestScrubNanos,
		SlowestScrubBytes: rep.SlowestScrubBytes,
	}
}

// RecordPanic persists a panic from any verb, including the ones with no resolved configuration.
// Uncounted: the consecutive count answers "how many COLLECTION runs failed in a row", and a
// one-shot verb crashing is not one of those. The stack stays on stderr, being the one diagnostic
// that can carry payload-derived strings.
func RecordPanic(verb string, cause any) {
	recordWithoutRuntime("", formats.FailurePanic, fmt.Sprintf("panic in %s: %v", verb, cause))
}

// RecordStartupFailure persists a collecting run that could not start. JudgeTick cannot
// serve these: it is a *Runtime method, and not having a Runtime is exactly the failure.
func RecordStartupFailure(verb, runID string, cause error) {
	if cause == nil {
		return
	}
	recordWithoutRuntime(runID, formats.FailureInit,
		fmt.Sprintf("%s could not start: %v", verb, cause))
}

// RecordUpdateFailure persists a self-update that did not happen. Uncounted: collection is not
// failing, so counting it would report a broken collector. It matters because self-update is the
// remediation channel -- an install that cannot replace itself cannot be fixed remotely, and this
// reached stderr only, which on a supervised daemon is a log file nothing ships.
func RecordUpdateFailure(message string) {
	recordWithoutRuntime("", formats.FailureUpdate, message)
}

func recordWithoutRuntime(runID, kind, message string) {
	appendWithoutRuntime("failure", func(dir string, rec *formats.FailureRecord) bool {
		ev := newEvent(dir, runID, kind, message)
		rec.Append(ev)
		if ev.Counted() {
			rec.ConsecutiveFailures++
		}
		return true
	})
}

// appendWithoutRuntime serves the paths with no resolved configuration. The state directory
// resolves the way the kill switch resolves it, so a config too broken to load cannot also hide
// the record of what broke.
func appendWithoutRuntime(what string, mutate func(dir string, rec *formats.FailureRecord) bool) {
	dir, _, err := pauseStateDir()
	if err != nil || dir == "" {
		return
	}
	// A verb can fail before anything has minted an identity, so the directory may not exist yet.
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return
	}
	rec := readFailureRecord(dir)
	if !mutate(dir, &rec) {
		return
	}
	if err := writeFailureRecord(dir, rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record the %s: %v\n", what, err)
	}
}

// Placeholdered here rather than at the call sites, so no path can reach the record by a route
// that forgot.
func newEvent(stateDir, runID, kind, message string) formats.FailureEvent {
	return formats.FailureEvent{
		At:      time.Now().UTC().Format(time.RFC3339),
		Kind:    kind,
		RunID:   runID,
		Message: formats.ApplyUserPlaceholder(message, engine.UsernameFromStateDir(stateDir)),
	}
}

// What the heartbeat carries: the in-memory failures, plus the crash read out of the journal.
func (r *Runtime) failureRecord() formats.FailureRecord {
	r.recMu.Lock()
	rec := *r.loadRecordLocked()
	rec.Recent = append([]formats.FailureEvent(nil), rec.Recent...)
	r.recMu.Unlock()
	rec.LastCrash = r.lastCrash
	return rec
}

// A record that does not parse is reported and then treated as absent: refusing to flush over
// corrupt bookkeeping would invert the priorities.
func readFailureRecord(stateDir string) formats.FailureRecord {
	raw, _, err := platform.ReadWhole(filepath.Join(stateDir, lastFailureFile), maxFailureBytes)
	if err != nil {
		// errors.Is, not os.IsNotExist: ReadWhole wraps the open error, and a fresh install's
		// missing record is the normal case, not a warning.
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: %s is unreadable and is ignored: %v\n", lastFailureFile, err)
		}
		return formats.FailureRecord{}
	}
	var rec formats.FailureRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s does not parse and is ignored: %v\n", lastFailureFile, err)
		return formats.FailureRecord{}
	}
	return rec
}

// The first reason is enough: a run that failed every upload almost always failed them all the
// same way, and the audit log has the rest.
func firstFailureReason(rep formats.Report) string {
	for _, s := range rep.Sources {
		for _, f := range s.Files {
			if f.Decision == formats.DecisionFailed && f.Reason != "" {
				return f.Reason
			}
		}
	}
	return "no reason recorded"
}

// writeFailureRecord is the one spelling of the write, mirroring readFailureRecord so the file name
// and its permissions are stated once.
func writeFailureRecord(stateDir string, rec formats.FailureRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return platform.WriteAtomic(filepath.Join(stateDir, lastFailureFile), body, 0o600)
}
