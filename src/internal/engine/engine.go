// Package engine is the flush loop: discover, detect change, read whole, scrub, seal,
// upload, commit. Retry is re-run: with no pending-upload state, a crash before the fingerprint
// commit re-ships onto the same path-derived key. Nothing is durable until that commit.
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// backoffCap bounds retry backoff. There is no max-retry: giving up is silent data loss.
const backoffCap = time.Hour

// Options configures one run.
type Options struct {
	// Plan is what the loop needs from the configuration, and nothing else.
	Plan     Plan
	Identity *identity.Unit
	Log      *auditlog.Log
	Registry *sources.Registry

	// Upload is the write path. Required for a run that ships; a preview seals without one.
	Upload UploadPort

	// Recipients the object is encrypted to; an enterprise deployment adds org recipients.
	Recipients []age.Recipient

	// Unbounded ignores max_files_per_run. Set by the drain only; see Run.
	Unbounded bool

	// DryRun reads, scrubs and seals but neither uploads nor commits. This is `preview`.
	DryRun bool

	// Enrichers is the compiled registry. Nil disables enrichment, and a source declaring one
	// is then refused rather than silently collected raw-only.
	Enrichers *transforms.Registry

	// Env expands an enricher's database candidates with the same ~ and $VAR rules as catalog roots.
	Env sources.Env

	// Heartbeat publishes discovery health after a run. Optional and best-effort: it fails open.
	Heartbeat func(context.Context, Report) error

	// Progress streams each file's outcome as it is decided. Optional; nil is silent.
	Progress formats.Progress

	// RunID is the process's crash-journal id, stamped into every manifest this run seals.
	RunID string

	// Now is injectable so tests are not timing-dependent.
	Now func() time.Time

	// CommitBatch bounds how many fingerprints buffer before the state document is replaced.
	// Zero takes the default; it is not configuration.
	CommitBatch int

	// Workers pins how many files a source pass computes at once. Zero takes GOMAXPROCS.
	Workers int

	// UploadWorkers pins how many PUTs are in flight at once. Zero takes eight times the
	// compute pool; see uploadConcurrency in pool.go.
	UploadWorkers int

	// Client is the build stamped into every manifest this run writes.
	Client transforms.Client

	// user is the placeholder username, set by Run before anything that reads it.
	user string

	// The run's compiled scrubber, shared by the raw and derived paths.
	scrub    *transforms.Scrubber
	scrubErr error
}

// The run's vocabulary lives in the contract layer, aliased here so an adapter that needs one
// of these types does not import the core.
type (
	FileOutcome   = formats.FileOutcome
	SourceOutcome = formats.SourceOutcome
	Report        = formats.Report
)

// Plan is the configuration the loop actually reads: every field is one it branches on. This
// keeps the core free of the config package and testable from a struct literal.
type Plan struct {
	// OrganizationID is the organization= key segment; empty means the standalone placeholder.
	OrganizationID string

	StateDir       string
	MaxFilesPerRun int

	Sources []sources.Resolved

	// RulePacks and StructuralEx configure redaction; the scrub floor is an adapter's decision.
	RulePacks      []string
	SecretKeyNames []string
	StructuralEx   map[string][]string

	// Deny is the compiled path deny list. Required: a nil deny list would read as "nothing is denied".
	Deny *sources.List

	// Ignore drops candidates of an ignored repository. Nil is the ordinary state and
	// means nothing is ignored.
	Ignore *sources.RepoFilter

	ConfigVersion int

	// ConfigExpired stamps every manifest. Expiry does not stop collection; it makes staleness visible.
	ConfigExpired bool
}

// Run performs one flush. Sources flush sequentially: the loop is poll-shaped, so a missed tick
// is a catch-up rather than a loss.
func Run(ctx context.Context, st *Store, o Options) (rep Report, err error) {
	// Commits batch for the run: an unflushed commit means the file ships again onto the same key.
	store := newCommitBuffer(st, o.CommitBatch)

	// Refusing here beats sealing every file and only then discovering there is nowhere to put them.
	if o.Upload == nil && !o.DryRun {
		return rep, errors.New("engine: no upload port; a run that ships needs one, and only " +
			"preview runs without it")
	}

	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.DryRun {
		// Decided once: a preview writes no audit lines, so every append below guards only on nil.
		o.Log = nil
	}
	o.user = UsernameFromStateDir(o.Plan.StateDir)

	// Runs first of the deferred summarization (LIFO): the run is not over while state is still being written.
	defer func() {
		rep.FinishedAt = o.Now()
		if o.scrub != nil {
			d, n := o.scrub.Slowest()
			rep.SlowestScrubNanos, rep.SlowestScrubBytes = int64(d), n
		}
		summarize(&rep)
	}()

	// Deferred so a cancelled or failed run still makes durable what it already shipped, without
	// letting a flush failure hide the reason the run stopped.
	defer func() {
		if ferr := store.Flush(); ferr != nil && err == nil {
			err = ferr
		}
	}()
	if len(o.Recipients) == 0 {
		// Encryption is mandatory in every version: an old client must be less capable, never less safe.
		return Report{}, errors.New("engine: no age recipients configured")
	}

	// After the report is built, not before: the assignment above replaces the whole struct.
	rep = Report{StartedAt: o.Now(), StoreCorrupt: st.Corrupt()}

	// The pause state is checked before anything is read. Preview is exempt: it ships and commits
	// nothing, and a paused owner may still see what would be collected.
	if !o.DryRun {
		if p := platform.Read(o.Plan.StateDir); p.Paused {
			rep.Paused = true
			rep.PauseReason = p.Reason
			return rep, nil
		}
	}

	// Redaction compiles once per run, not once per file or object, and only after the pause
	// gate: a paused tick must stay cheap. The compiled scrubber is immutable, so every pass's
	// goroutines share it; a compile failure still parks each candidate.
	o.scrub, o.scrubErr = o.scrubber()

	budget := o.Plan.MaxFilesPerRun
	if o.Unbounded {
		// A drain must flush EVERYTHING pending; its ctx deadline bounds the work instead.
		budget = math.MaxInt
	}

	for _, src := range o.Plan.Sources {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		out := SourceOutcome{
			SourceID: src.ID,
			Family:   src.Family,
			Root:     src.Root,
			// A source that declares an emit key writes its own file rather than finding one.
			Emitted: src.Emit != "",
		}
		if !src.Enabled {
			out.Health = sources.AgentAbsent
			out.Reason = "disabled by configuration"
			rep.Sources = append(rep.Sources, out)
			continue
		}

		prim, err := o.Registry.For(src.Gather)
		if err != nil {
			out.Health = sources.MatchPresentUnreadable
			out.Reason = err.Error()
			rep.Sources = append(rep.Sources, out)
			continue
		}

		disc, err := prim.Discover(sources.Request{
			Source:   src,
			All:      o.Plan.Sources,
			Deny:     o.Plan.Deny,
			Ignore:   o.Plan.Ignore,
			StateDir: o.Plan.StateDir,
			Username: o.user,
			Now:      o.Now,
		})
		if err != nil {
			out.Health = sources.MatchPresentUnreadable
			out.Reason = err.Error()
			rep.Sources = append(rep.Sources, out)
			continue
		}
		out.Health = disc.Health
		out.Sniff = disc.Sniff
		out.AgentVersion = disc.AgentVersion
		out.Reason = disc.Reason

		// A spec change means this source is read differently, so its old state describes nothing.
		// Preview persists nothing and masks the stale entries instead, staying equal to a real sync.
		if o.DryRun {
			store.PreviewSpec(src.ID, src.SpecFingerprint)
		} else {
			if _, err := store.EnsureSpec(src.ID, src.SpecFingerprint); err != nil {
				return rep, err
			}
		}

		// One audit line per run rather than per path: a thousand denied paths under one unreadable
		// parent are one fact, not a thousand.
		out.Unreadable = disc.Unreadable
		out.UnreadableExample = disc.UnreadableExample
		out.UnreadableReason = disc.UnreadableReason
		if out.Unreadable > 0 && o.Log != nil {
			_ = o.Log.Append(auditlog.Entry{
				Decision:      auditlog.DecisionSkipped,
				SourceID:      src.ID,
				File:          disc.UnreadableExample,
				ConfigVersion: o.Plan.ConfigVersion,
				Reason:        "not readable during discovery: " + disc.UnreadableReason,
			})
		}

		// Files the size cap kept out, audited one line each: "will never read it" is a decision.
		out.Oversize = len(disc.Oversize)
		for _, big := range disc.Oversize {
			if big.Size > out.OversizeLargest {
				out.OversizeLargest, out.OversizeExample, out.OversizeLimit = big.Size, big.RelPath, big.Limit
			}
			if o.Log == nil {
				continue
			}
			_ = o.Log.Append(auditlog.Entry{
				Decision:      auditlog.DecisionSkipped,
				SourceID:      src.ID,
				File:          big.RelPath,
				BytesIn:       big.Size,
				ConfigVersion: o.Plan.ConfigVersion,
				Reason: fmt.Sprintf("over the size cap: %d bytes, limit %d — not read",
					big.Size, big.Limit),
			})
		}

		enrichers := o.enrichersFor(src)

		// Staged raw units live in memory for this source's pass only: there is no spool.
		pass := &sourcePass{
			o:        o,
			store:    store,
			src:      src,
			disc:     disc,
			rep:      &rep,
			out:      &out,
			budget:   &budget,
			scrubber: o.scrub,
			scrubErr: o.scrubErr,
			staging:  len(enrichers) > 0,
		}
		if err := pass.run(ctx); err != nil {
			// A refusal and an unavailable control plane both carry a source outcome worth
			// reporting; a cancelled context, the only other way this returns, does not.
			if errorKind(err) != "upload" {
				rep.Sources = append(rep.Sources, out)
			}
			return rep, err
		}
		staged := pass.stagedUnits()

		// Enrichment runs only after every raw unit has shipped: an enricher cannot abort, park or
		// delay a raw file. A unit-free one runs every flush, gated only by shipDerived's output hash.
		for _, enricher := range enrichers {
			if len(staged) == 0 && enricher.NeedsUnits() {
				continue
			}
			err := o.enrichSource(ctx, store, src, enricher, staged, &out, &rep)
			if err == nil {
				continue
			}
			rep.Sources = append(rep.Sources, out)
			return rep, err
		}

		// Forget files this source no longer has. Only a source that COLLECTED and returned
		// candidates proves absence, so a run the budget truncated forgets nothing.
		if !o.DryRun && out.Health == sources.Collected && len(disc.Candidates) > 0 && out.Remaining == 0 {
			live := make(map[string]bool, len(disc.Candidates))
			for _, c := range disc.Candidates {
				live[c.Path] = true
			}
			// Derived entries never appear among candidates, so they are kept by adding them here.
			for _, f := range out.Files {
				live[f.NativePath] = true
			}
			if n, derr := store.DropVanished(src.ID, live); derr != nil {
				return rep, derr
			} else if n > 0 {
				if o.Log != nil {
					_ = o.Log.Append(auditlog.Entry{
						Decision:      auditlog.DecisionSkipped,
						SourceID:      src.ID,
						ConfigVersion: o.Plan.ConfigVersion,
						Reason: fmt.Sprintf(
							"forgot %d fingerprint(s) for files this source no longer has", n),
					})
				}
			}
		}

		// A source boundary bounds what a crash re-ships to the source in flight.
		if err := store.Flush(); err != nil {
			return rep, err
		}

		rep.Sources = append(rep.Sources, out)
	}

	slices.SortFunc(rep.Sources, func(a, b SourceOutcome) int { return cmp.Compare(a.SourceID, b.SourceID) })

	if !o.DryRun && o.Heartbeat != nil {
		// Health reporting fails open; only redaction fails closed.
		err := o.Heartbeat(ctx, rep)
		if err != nil && o.Log != nil {
			_ = o.Log.Append(auditlog.Entry{
				Decision: auditlog.DecisionFailed,
				Reason:   "heartbeat write failed: " + err.Error(),
			})
		}
	}
	return rep, nil
}

// summarize totals bytes from the outcomes rather than accumulating: derived.go appends outcomes
// without passing through the fold. Emitted and derived are excluded so an idle run totals zero.
func summarize(rep *Report) {
	var shippedIn []int64
	for _, s := range rep.Sources {
		if s.Emitted {
			continue
		}
		for _, f := range s.Files {
			if f.Derived {
				continue
			}
			rep.BytesRead += f.BytesIn
			rep.BytesSealed += f.BytesOut
			if f.Decision == formats.DecisionShipped {
				shippedIn = append(shippedIn, f.BytesIn)
			}
		}
	}
	rep.MedianFileBytes = median(shippedIn)
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	sorted := slices.Clone(v)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

func (o Options) scrubber() (*transforms.Scrubber, error) {
	cfg := transforms.DefaultConfig()
	cfg.RulePacks = o.Plan.RulePacks
	// Additive: configuration can only lengthen the compiled default list, never replace it.
	cfg.SecretKeyNames = append(cfg.SecretKeyNames, o.Plan.SecretKeyNames...)
	cfg.Exemptions = o.Plan.StructuralEx
	cfg.Username = o.user
	return transforms.New(cfg)
}

// orgOf is the organization= key segment, always the RESOLVED value: a hardcoded "default" splits
// one enrolled install across two organization subtrees. The fallback keeps key depth constant.
func orgOf(p Plan) string {
	if p.OrganizationID == "" {
		return "default"
	}
	return p.OrganizationID
}

func isJSONL(src sources.Resolved) bool {
	return src.Sniff != nil && src.Sniff.Kind == "jsonl"
}

// backoffFor computes the next attempt delay: a minute, doubling, capped at an hour, with up to
// 12.5% of jitter subtracted so correlated failures do not wake together. Derived from attempt and
// spread rather than rand, so runs stay reproducible, and subtracted so the cap stays a ceiling.
func backoffFor(attempt int, spread uint64) time.Duration {
	d := min(time.Minute<<min(attempt, 8), backoffCap)
	// Up to an eighth off, so the spread is visible without meaningfully shortening the delay.
	jitter := time.Duration(spread%9) * d / 64
	return d - jitter
}
