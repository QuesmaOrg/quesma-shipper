package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Enrichment: running an enricher and shipping what it derived. Every raw unit has shipped and
// committed first, so an enricher cannot abort, park or delay a raw file. A derived object takes
// the IDENTICAL path a raw file takes, being built from a database that holds auth material.

// recomputeWindow is how recently a source file must have changed for its enrichment to be
// recomputed anyway: the DB side moves on its own, and a late tool result would never be collected.
const recomputeWindow = 24 * time.Hour

// enrichersFor returns every enabled enricher of a source, sorted by id because src.Enrichers is a
// map: two flushes of the same config must run the same enrichers in the same order.
func (o Options) enrichersFor(src sources.Resolved) []transforms.Enricher {
	if o.Enrichers == nil {
		return nil
	}
	ids := make([]string, 0, len(src.Enrichers))
	for id, on := range src.Enrichers {
		if on {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out []transforms.Enricher
	for _, id := range ids {
		e, err := o.Enrichers.For(id)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// enrichSource runs one enricher and ships whatever it derived. A non-nil error is the run's:
// the install was refused, or the control plane could not authorize.
func (o Options) enrichSource(
	ctx context.Context,
	store *commitBuffer,
	src sources.Resolved,
	e transforms.Enricher,
	staged []transforms.RawUnit,
	out *SourceOutcome,
	rep *Report,
) error {
	out.EnricherID = e.ID()
	out.EnricherVersion = e.Version()

	// The first declared database candidate that exists; empty means absent, which is not an error.
	dbPath := o.Env.FirstExistingFile(e.DBCandidates())
	res := e.Enrich(transforms.Input{
		Units:      staged,
		DBPath:     dbPath,
		ScratchDir: filepath.Join(o.Plan.StateDir, "scratch"),
	})

	out.EnrichSkipped += res.Skipped
	out.EnrichMismatch += res.Mismatched
	out.EnrichErrors += res.Errors
	out.EnrichNotes = append(out.EnrichNotes, res.Notes...)
	out.EnrichInfos = append(out.EnrichInfos, res.Infos...)
	rep.EnrichMismatch += res.Mismatched

	// A mismatch gets its own audit entry: a lost window must appear there, not only in a
	// counter. Notes only — an info describes an object that ships, so stamping it "skipped"
	// would record loss that did not happen.
	if o.Log != nil {
		for _, note := range res.Notes {
			_ = o.Log.Append(auditlog.Entry{
				Decision:      auditlog.DecisionSkipped,
				SourceID:      src.ID,
				ConfigVersion: o.Plan.ConfigVersion,
				Reason:        "enrich: " + note,
			})
		}
	}

	shipped, halted := o.shipDerivedGroups(ctx, store, src, e, dbPath, res.Objects, out)
	out.Enriched += shipped
	rep.Shipped += shipped
	if halted != nil {
		// The same line the raw pass records, so the outcome itself says why the run stopped.
		out.Reason = "uploads stopped: " + halted.Error()
		if o.Log != nil {
			_ = o.Log.Append(auditlog.Entry{
				Decision:      auditlog.DecisionFailed,
				SourceID:      src.ID,
				ConfigVersion: o.Plan.ConfigVersion,
				Reason:        out.Reason,
			})
		}
		return fmt.Errorf("enricher %s stopped: %w", e.ID(), halted)
	}
	return nil
}

// shipDerivedGroups ships everything one enricher derived. Outcomes are index-addressed so the
// report keeps enricher order; a non-nil halted means this enricher stopped uploading.
func (o Options) shipDerivedGroups(
	ctx context.Context,
	store *commitBuffer,
	src sources.Resolved,
	e transforms.Enricher,
	dbPath string,
	objects []transforms.Derived,
	out *SourceOutcome,
) (shipped int, halted error) {
	fos := make([]FileOutcome, len(objects))
	group := &batcher{
		maxObjects: maxBatchObjects,
		send: func(items []stagedUpload) {
			outcomes := o.authorizeAndUpload(ctx, items)
			for i, g := range items {
				idx := g.res.idx
				fos[idx] = o.commitDerived(store, g.res.outcome, g.pending, outcomes[i])
				if fos[idx].Decision == auditlog.DecisionShipped {
					shipped++
				}
				// A refusal or an unavailable plane stops this enricher; a failed PUT does not.
				if stopsRun(outcomes[i]) && halted == nil {
					halted = outcomes[i]
				}
			}
		},
	}

	for i, d := range objects {
		if halted != nil {
			fos[i] = FileOutcome{
				SourceID: src.ID, NativePath: d.NativePath, BytesIn: int64(len(d.Payload)), Derived: true,
				Decision: auditlog.DecisionFailed, Reason: "not attempted: " + halted.Error(),
			}
			continue
		}
		fo, pending := o.prepareDerived(store, src, e, dbPath, d)
		if pending == nil {
			fos[i] = fo
			continue
		}
		group.add(stagedUpload{res: fileResult{idx: i, outcome: fo}, pending: pending}, int64(len(pending.obj)))
	}
	group.flush()

	out.Files = append(out.Files, fos...)
	return shipped, halted
}

// commitDerived turns one object's verdict into its outcome, committing only after the PUT.
func (o Options) commitDerived(
	store *commitBuffer, fo FileOutcome, pending *pendingPut, oc error,
) FileOutcome {
	present := errors.Is(oc, ErrAlreadyPresent)
	if oc != nil && !present {
		fo.Decision = auditlog.DecisionFailed
		fo.Reason = oc.Error()
		return fo
	}
	if err := store.Commit(pending.key, pending.next); err != nil {
		fo.Decision = auditlog.DecisionFailed
		fo.Reason = "derived upload succeeded but commit failed: " + err.Error()
		return fo
	}
	fo.Decision = auditlog.DecisionShipped
	if present {
		fo.Reason = alreadyPresentReason
	}
	return fo
}

// prepareDerived is the derived object's compute leg: change detection, scrub, key, manifest, seal.
// A non-nil pending means the object wants the network.
func (o Options) prepareDerived(
	store *commitBuffer,
	src sources.Resolved,
	e transforms.Enricher,
	dbPath string,
	d transforms.Derived,
) (fo FileOutcome, pending *pendingPut) {
	fo = FileOutcome{SourceID: src.ID, NativePath: d.NativePath, BytesIn: int64(len(d.Payload)), Derived: true}
	fail := func(reason string) (FileOutcome, *pendingPut) {
		fo.Decision = auditlog.DecisionFailed
		fo.Reason = reason
		return fo, nil
	}

	key := Key{
		SourceID:   src.ID,
		NativePath: d.NativePath,
	}
	fp, seen := store.Get(key)

	// Determinism supplies the change signal: an unchanged output hash means nothing to upload.
	if seen && fp.OutputHash == d.OutputHash && fp.SourceHash != "" {
		fo.Decision = auditlog.DecisionUnchanged
		fo.Reason = "enricher output hash unchanged"
		return fo, nil
	}

	if o.scrubErr != nil {
		return fail(o.scrubErr.Error())
	}
	// Redaction fails CLOSED here too: a derived object whose scrub failed does not ship.
	res, err := o.scrub.Scrub(d.Payload, transforms.Hint{Family: src.Family, JSONL: true})
	if err != nil {
		return fail("scrub failed closed on derived payload: " + err.Error())
	}

	relPath := d.NativePath
	if src.Root != "" && strings.HasPrefix(relPath, src.Root) {
		relPath = strings.TrimPrefix(strings.TrimPrefix(relPath, src.Root), string(filepath.Separator))
	}
	objectKey, err := o.mirrorKey(src.ID, relPath)
	if err != nil {
		return fail(err.Error())
	}
	fo.ObjectKey = objectKey

	sourceHash := transforms.Hash(d.Payload)
	ref := &EnricherRef{ID: e.ID(), Version: e.Version()}

	m := o.baseManifest(src, d.NativePath)
	m.SourceHash = sourceHash
	m.ShapeSniff = string(sources.SniffOK)

	// What makes this object distrustable: downstream cannot regenerate the DB-side fields.
	m.Derived = true
	m.Enricher = ref
	m.DerivedFrom = d.DerivedFrom
	m.EnrichStatus = string(d.Status)
	m.EnrichMismatches = d.Mismatches
	// The explained shortfalls, so a partial-but-ok object is identifiable without
	// parsing flush notes. omitempty keeps a complete object's manifest as it was.
	m.EnrichRepeats = d.Repeats
	m.EnrichTail = d.Tail
	m.EnrichAmbiguous = d.Ambiguous
	m.EnrichLineDecodeErrors = d.LineDecodeErrors
	m.DBProvenance = &transforms.DBProvenance{
		DBPath:     formats.ApplyUserPlaceholder(dbPath, o.user),
		ReadMethod: d.DBReadMethod,
		Keyspaces:  d.DBKeyspaces,
		RowsRead:   d.DBRowsRead,
	}
	m.Redaction = &transforms.RedactionSummary{
		Density:  res.Density(),
		RuleHits: res.RuleHits,
	}

	obj, sealed, err := transforms.Seal(m, res.Out, o.Recipients)
	if err != nil {
		return fail(err.Error())
	}
	fo.BytesOut = int64(len(obj))

	if o.DryRun {
		fo.Decision = auditlog.DecisionShipped
		fo.Reason = "would ship derived object (preview)"
		return fo, nil
	}

	return fo, &pendingPut{
		key:       key,
		objectKey: objectKey,
		obj:       obj,
		// ObjectMetadata carries derived=true in plaintext, so an erasure sweep needs only a HEAD.
		md: sealed.ObjectMetadata(),
		next: Fingerprint{
			SourceHash: sourceHash,
			OutputHash: d.OutputHash,
			Enricher:   ref,
		},
	}
}
