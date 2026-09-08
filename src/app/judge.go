package app

// The install's durable memory of its failures, carried by the next heartbeat that manages to
// ship. A failed flush uploads nothing, its own heartbeat included, so without this a machine that
// fails every tick is indistinguishable from an idle one.

import (
	"encoding/json"
	"errors"
	"fmt"
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
// Best-effort throughout: bookkeeping must never change a run's outcome.
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

	rec := readFailureRecord(r.eff.StateDir)
	rec.Facts = r.runFacts(rep, mem)

	// Recorded, never counted: the discard costs one re-ship rather than failing the run, but it
	// is the one condition that can otherwise lose a file for good.
	if rep.StoreCorrupt {
		rec.Append(newEvent(r.eff.StateDir, r.runID, formats.FailureStoreCorrupt,
			"the fingerprint store failed its checksum and was discarded; everything re-ships once"))
	}

	if err == nil {
		// No early return any more: the facts above change every run, and a healthy run's cost is
		// the baseline that makes the next one's readable.
		rec.ConsecutiveFailures = 0
	} else {
		rec.Append(newEvent(r.eff.StateDir, r.runID, kind, err.Error()))
		rec.ConsecutiveFailures++
	}

	if werr := writeFailureRecord(r.eff.StateDir, rec); werr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record the tick outcome: %v\n", werr)
	}
	return err
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
	recordWithoutRuntime("", formats.FailurePanic, fmt.Sprintf("panic in %s: %v", verb, cause), false)
}

// RecordStartupFailure persists a collecting run that could not start. JudgeTick cannot
// serve these: it is a *Runtime method, and not having a Runtime is exactly the failure. Counted,
// unlike a verb panic -- a collecting run that cannot start is one that failed.
func RecordStartupFailure(verb, runID string, cause error) {
	if cause == nil {
		return
	}
	recordWithoutRuntime(runID, formats.FailureInit,
		fmt.Sprintf("%s could not start: %v", verb, cause), true)
}

// RecordUpdateFailure persists a self-update that did not happen. Uncounted: collection is not
// failing, so counting it would report a broken collector. It matters because self-update is the
// remediation channel -- an install that cannot replace itself cannot be fixed remotely, and this
// reached stderr only, which on a supervised daemon is a log file nothing ships.
func RecordUpdateFailure(message string) {
	recordWithoutRuntime("", formats.FailureUpdate, message, false)
}

// recordWithoutRuntime serves the paths with no resolved configuration. The state directory
// resolves the way the kill switch resolves it, so a config too broken to load cannot also hide
// the record of what broke.
func recordWithoutRuntime(runID, kind, message string, counted bool) {
	dir, _, err := pauseStateDir()
	if err != nil || dir == "" {
		return
	}
	// A verb can fail before anything has minted an identity, so the directory may not exist yet.
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return
	}
	rec := readFailureRecord(dir)
	rec.Append(newEvent(dir, runID, kind, message))
	if counted {
		rec.ConsecutiveFailures++
	}
	// Silent: the caller is already on its way out with something to print.
	_ = writeFailureRecord(dir, rec)
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

// What the heartbeat carries: the persisted failures, plus the crash read out of the journal.
func (r *Runtime) failureRecord() formats.FailureRecord {
	rec := readFailureRecord(r.eff.StateDir)
	rec.LastCrash = r.lastCrash
	return rec
}

// A record that does not parse is reported and then treated as absent: refusing to flush over
// corrupt bookkeeping would invert the priorities.
func readFailureRecord(stateDir string) formats.FailureRecord {
	raw, _, err := platform.ReadWhole(filepath.Join(stateDir, lastFailureFile), maxFailureBytes)
	if err != nil {
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
