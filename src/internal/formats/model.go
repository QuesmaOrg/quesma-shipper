// Package formats holds the vocabulary every layer shares: these words appear in the heartbeat
// document and the audit log, so they are wire values. The vocabulary types below depend on
// nothing but the standard library; a type belongs here only when several layers must agree on it
// and none owns it.
package formats

import (
	"errors"
	"time"
)

// ErrCredentialsRefused means the control plane or the store rejected this install's identity.
// It applies to the install rather than to one file, so the run must stop instead of retrying.
var ErrCredentialsRefused = errors.New("this install's credentials were refused")

// HealthState is a source's discovery state. A closed enum rather than free text because a
// missing agent and a root whose globs match nothing look alike but only one is drift.
type HealthState string

const (
	// AgentAbsent means no root resolved: expected silence.
	AgentAbsent HealthState = "agent_absent"

	// RootPresentNoMatch means the root exists but the globs matched nothing: probable drift.
	RootPresentNoMatch HealthState = "root_present_no_match"

	// MatchPresentUnreadable means files were found but could not be used.
	MatchPresentUnreadable HealthState = "match_present_unreadable"

	// Collected is the normal case.
	Collected HealthState = "collected"
)

// SniffResult is the closed enum. Shape, never semantics.
type SniffResult string

const (
	SniffOK              SniffResult = "ok"
	SniffEmpty           SniffResult = "empty"
	SniffUnexpectedShape SniffResult = "unexpected_shape"
	SniffUnreadable      SniffResult = "unreadable"
)

// Decision is what the client decided about one file. It is the audit log's vocabulary.
type Decision string

const (
	// DecisionShipped: sealed and written to the sink.
	DecisionShipped Decision = "shipped"

	// DecisionUnchanged: the content hash matched, so nothing was re-shipped.
	DecisionUnchanged Decision = "unchanged"

	// DecisionSkipped: not collected, with a reason.
	DecisionSkipped Decision = "skipped"

	// DecisionParked: held off behind a backoff, matching the fingerprint's parked flag.
	DecisionParked Decision = "parked"

	// DecisionFailed: the attempt failed and will be re-run. Nothing was committed.
	DecisionFailed Decision = "failed"
)

// FileOutcome is what happened to one file.
type FileOutcome struct {
	// Fatal marks a failure about the install rather than the file; the run stops on it.
	Fatal bool

	// Derived payloads count in the file counters but stay out of byte totals: nothing was read off disk.
	Derived bool

	SourceID   string
	NativePath string
	RelPath    string
	ObjectKey  string
	Decision   Decision
	BytesIn    int64
	BytesOut   int64
	Density    float64
	RuleHits   map[string]int
	Reason     string
}

// Progress receives one file's outcome as the run decides it, done of total for that source.
// A status carrier, not part of the data path: nil means silent.
type Progress func(sourceID string, done, total int, f FileOutcome)

// SourceOutcome is one source's contribution to a run.
type SourceOutcome struct {
	SourceID     string
	Family       string
	Health       HealthState
	Sniff        SniffResult
	AgentVersion string
	Reason       string
	Root         string

	// Remaining counts candidates this pass never started: budget exhausted or byte gate held.
	Remaining int

	// Emitted marks files the shipper generated itself; they ship but stay out of byte totals.
	Emitted bool

	// Oversize counts files the size cap kept out; they are never read and never shipped.
	Oversize int

	// The worst offender: a count alone cannot say whether the cap met one pathological file.
	OversizeLargest int64
	OversizeExample string
	OversizeLimit   int64

	// Unreadable counts paths the walk could not look at, reported even when the source collected.
	Unreadable        int
	UnreadableExample string
	UnreadableReason  string
	Files             []FileOutcome

	// Enrich counters. A mismatch loses that window's DB-side fields until the join is fixed; a skip does not.
	Enriched        int
	EnrichSkipped   int
	EnrichMismatch  int
	EnrichErrors    int
	EnrichNotes     []string
	EnricherID      string
	EnricherVersion int

	// Informational enrich notes: explained store shortfalls on objects that shipped. Apart
	// from EnrichNotes so a renderer can tell loss from commentary without parsing text.
	EnrichInfos []string
}

// FailureRecord is what an install remembers about going wrong. It is persisted locally and
// carried by the next heartbeat that manages to ship, because a failed run uploads nothing -- its
// own heartbeat included -- so without this every failure is invisible downstream the moment the
// machine recovers, and a machine that fails every run looks exactly like an idle one.
//
// Two counters, not one: a crash and a failed run are different events, counted by different
// mechanisms. Crashes are read back out of the crash journal as far as the last delivered report;
// failures are incremented per run and reset on success. Collapsing them into one integer would
// have to pick one of those reset rules and be wrong about the other.
type FailureRecord struct {
	// A run that died without being able to say anything, detected by a journal with no "exit".
	LastCrash *LastCrash `json:"last_crash,omitempty"`

	// Oldest first. Bounded rather than complete because it rides a document that ships every run;
	// consecutive heartbeats overlap, so a version nobody read loses nothing.
	Recent []FailureEvent `json:"recent_failures,omitempty"`

	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`

	// Overwritten every run, unlike the log above. None of it is a failure; it is what makes one
	// explicable, and what a machine that is degrading rather than erroring shows first.
	Facts *RunFacts `json:"facts,omitempty"`
}

// RunFacts is the last run's cost and the limits it ran under. Memory pressure and a pathological
// redaction are the two ways an install degrades without any single step returning an error, so
// they are recorded as figures rather than events: they would otherwise recur every tick and evict
// the log. The concurrency knobs travel with them because the same figures mean different things at
// different widths.
type RunFacts struct {
	GOMAXPROCS       int   `json:"gomaxprocs,omitempty"`
	MaxFilesPerRun   int   `json:"max_files_per_run,omitempty"`
	MaxInFlightBytes int64 `json:"max_in_flight_bytes,omitempty"`

	// SoftLimitBytes is the ceiling Go is holding the heap under; HeapInuseBytes against it is the
	// pressure. GCCycles rises sharply as the two converge, which is the earlier signal.
	SoftLimitBytes int64  `json:"soft_limit_bytes,omitempty"`
	HeapInuseBytes uint64 `json:"heap_inuse_bytes,omitempty"`
	SysBytes       uint64 `json:"sys_bytes,omitempty"`
	GCCycles       uint32 `json:"gc_cycles,omitempty"`

	SlowestScrubNanos int64 `json:"slowest_scrub_nanos,omitempty"`
	SlowestScrubBytes int64 `json:"slowest_scrub_bytes,omitempty"`
}

// Twenty is about five hours of a daemon failing every tick, and keeps the document a few KB.
const MaxRecentFailures = 20

// Failure kinds. A closed set, so a reader can group without parsing prose.
const (
	FailureTick  = "tick_failed" // a run that failed, or returned nil having shipped nothing
	FailurePanic = "panic"       // a verb that panicked; the stack stays on the machine

	// The run continues from an empty store, so this is reported without being counted.
	FailureStoreCorrupt = "store_corrupt"

	// Counted: an install that cannot start is not collecting at all.
	FailureInit = "init_failed"

	// Uncounted, and the one class where the run around it is fine: collection succeeded, but the
	// install cannot replace itself, so it is frozen on this version and no fix can reach it.
	FailureUpdate = "update_failed"

	// FailureShutdown is the SIGTERM drain, the last slice before exit. Same mechanism as a tick,
	// separate kind because the consequence differs: a tick re-ships next tick, and on a host about
	// to disappear this one does not.
	FailureShutdown = "shutdown_failed"
)

// The message is username-placeholdered like every outbound diagnostic; a stack stays local.
type FailureEvent struct {
	At   string `json:"at"`
	Kind string `json:"kind"`

	// Twenty events from twenty runs must read differently from one run that failed twenty times.
	// Empty for a panic, recorded from outside the crash journal before any id is minted.
	RunID   string `json:"run_id,omitempty"`
	Message string `json:"message"`
}

// Not a ring buffer: this is serialized whole on every write, so the simple form is the cheap one.
func (r *FailureRecord) Append(e FailureEvent) {
	r.Recent = append(r.Recent, e)
	if n := len(r.Recent); n > MaxRecentFailures {
		r.Recent = append(r.Recent[:0], r.Recent[n-MaxRecentFailures:]...)
	}
}

// Latest is the newest event, or nil on an install that has never failed.
func (r *FailureRecord) Latest() *FailureEvent {
	if len(r.Recent) == 0 {
		return nil
	}
	return &r.Recent[len(r.Recent)-1]
}

// LastCrash reports how the previous run died, fleet-visibly: the run id and the lifecycle step it
// had reached, never a path. Attributing a death to the FILE being read cost a journal line per
// file, which was 92% of everything the journal wrote, so the phase is as fine as this gets.
type LastCrash struct {
	RunID       string `json:"run_id,omitempty"`
	Phase       string `json:"phase,omitempty"`
	Consecutive int    `json:"consecutive,omitempty"`
}

// Report is what one run did, for status, preview output and the heartbeat.
type Report struct {
	StartedAt time.Time

	// FinishedAt is the denominator of every rate the CLI prints; zero suppresses those lines.
	FinishedAt time.Time

	Sources []SourceOutcome

	Shipped   int
	Unchanged int
	Skipped   int
	Parked    int
	Failed    int

	// Truncated is true when max_files_per_run cut the run short.
	Truncated bool

	// Remaining is the run's backlog: candidates no source got to. Only what was NOT attempted,
	// so parked and oversize stay separate; they are not a speed problem.
	Remaining int

	// BytesRead is what was read off the machine, BytesSealed what left it after sealing, and
	// MedianFileBytes the middle shipped file's input size: a mean would follow one huge transcript.
	BytesRead       int64
	BytesSealed     int64
	MedianFileBytes int64

	// EnrichMismatch is the fleet-wide alarm total: there is no raw-row fallback.
	EnrichMismatch int

	// Paused says the pause state stopped the run; PauseReason carries what the operator wrote.
	Paused      bool
	PauseReason string

	// StoreCorrupt says this run opened a fingerprint document that failed its own checksum and
	// started from empty instead. Collection was unaffected; everything re-ships once. Carried on
	// the report because detecting it silently would leave the one true silent-loss hole reported
	// to nobody.
	StoreCorrupt bool

	// SlowestScrubNanos is the worst single redaction this run served, with the payload behind it.
	// A pattern that backtracks pathologically stalls an install without erroring anywhere.
	SlowestScrubNanos int64
	SlowestScrubBytes int64
}
