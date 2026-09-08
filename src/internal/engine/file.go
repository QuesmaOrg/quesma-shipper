package engine

import (
	"hash/fnv"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Per-file collection. The order is the invariant: pre-filter, content hash, scrub, seal, PUT,
// and only then the commit, so a crash above it re-runs the file onto the same key rather than
// losing it. The seal/PUT split exists because the two legs wait on different slots.

// pendingPut is a sealed object waiting for an upload slot, so the compute slot can be released.
type pendingPut struct {
	key       Key
	objectKey string
	obj       []byte
	md        map[string]string

	// next is the fingerprint a successful PUT commits; fresh rather than copied, so Attempts resets.
	next Fingerprint
}

// prepareFile is the compute leg: change detection, read, scrub, seal. A non-nil pendingPut
// means the file wants the network.
func (o Options) prepareFile(
	job fileJob,
	src sources.Resolved,
	disc sources.Discovery,
	scrubber *transforms.Scrubber,
	scrubErr error,
	staging bool,
) (res fileResult, pending *pendingPut) {
	cand := job.cand
	res = fileResult{idx: job.idx, bytes: cand.Size}
	out := &res.outcome
	*out = FileOutcome{
		SourceID:   src.ID,
		NativePath: cand.Path,
		RelPath:    cand.RelPath,
	}

	key := Key{
		SourceID:   src.ID,
		NativePath: cand.Path,
	}
	fp, seen := job.fp, job.seen

	// A parked entry waits out its backoff. Never an unconditional retry.
	if fp.Parked && o.Now().Before(fp.BackoffUntil) {
		out.Decision = auditlog.DecisionSkipped
		out.Reason = "parked until " + fp.BackoffUntil.Format(time.RFC3339) + ": " + fp.LastError
		return res, nil
	}

	// Cheap pre-filter on size and mtime only: mtime alone re-ships byte-identical files, so the
	// content hash below stays the authority. A non-empty SourceHash marks a committed ship.
	if seen && fp.SourceSize == cand.Size && fp.SourceMTime.Equal(cand.MTime) && fp.SourceHash != "" &&
		!o.withinRecomputeWindow(staging, cand) {
		out.Decision = auditlog.DecisionUnchanged
		out.Reason = "size and mtime unchanged"
		return res, nil
	}

	raw, info, err := platform.ReadWhole(cand.Path, src.MaxFileBytes)
	if err != nil {
		failAndBackOff(o, &res, key, fp, err.Error())
		return res, nil
	}
	out.BytesIn = int64(len(raw))
	sourceHash := transforms.Hash(raw)

	// The enricher sees exactly the bytes that shipped, never a file it re-opened mid-append.
	if staging {
		res.unit = &transforms.RawUnit{
			NativePath: cand.Path,
			Content:    raw,
			SourceHash: sourceHash,
		}
	}

	// The content hash is the authority: an mtime-only change refreshes the stat and ships nothing.
	if seen && sourceHash == fp.SourceHash {
		out.Decision = auditlog.DecisionUnchanged
		out.Reason = "content hash unchanged"
		if !o.DryRun {
			refreshed := fp
			refreshed.SourceSize = cand.Size
			refreshed.SourceMTime = cand.MTime
			// Whatever parked this entry is over: the read just succeeded and its hash matches a
			// hash only a completed ship could have written. Carrying the flag forward reports a
			// healthy file as parked for good, and re-reads it every tick, since the backoff it
			// would wait on has already expired.
			refreshed.Parked = false
			refreshed.LastError = ""
			refreshed.BackoffUntil = time.Time{}
			refreshed.Attempts = 0
			res.intent = intent{kind: intentRefresh, key: key, fp: refreshed}
		}
		return res, nil
	}

	// Drift signal only: the whole file ships regardless, but truncation stops looking like growth.
	if seen && cand.Size < fp.SourceSize {
		out.Reason = "file shrank: truncation or rewrite"
	}

	if scrubErr != nil {
		failAndBackOff(o, &res, key, fp, scrubErr.Error())
		return res, nil
	}
	scrubbed, err := scrubber.Scrub(raw, transforms.Hint{Family: src.Family, JSONL: isJSONL(src)})
	if err != nil {
		// Fail closed: a scrub-ENGINE error means this file does not upload.
		failAndBackOff(o, &res, key, fp, "scrub failed closed: "+err.Error())
		return res, nil
	}
	out.Density = scrubbed.Density()
	out.RuleHits = scrubbed.RuleHits
	raw = nil

	objectKey, err := o.mirrorKey(src.ID, cand.RelPath)
	if err != nil {
		out.Decision = auditlog.DecisionFailed
		out.Reason = err.Error()
		return res, nil
	}
	out.ObjectKey = objectKey

	manifest := o.manifestFor(src, cand, disc, sourceHash, info.ModTime(), scrubbed)
	// Set BEFORE sealing: Seal takes the manifest by value, and this hash travels in metadata.
	manifest.ShippedHash = transforms.Hash(scrubbed.Out)
	obj, err := transforms.Seal(manifest, scrubbed.Out, o.Recipients)
	if err != nil {
		out.Decision = auditlog.DecisionFailed
		out.Reason = err.Error()
		return res, nil
	}
	out.BytesOut = int64(len(obj))

	if o.DryRun {
		// preview stops here: everything that would leave the machine is computed, and nothing has.
		out.Decision = auditlog.DecisionShipped
		if out.Reason == "" {
			out.Reason = "would ship (preview)"
		}
		return res, nil
	}

	return res, &pendingPut{
		key:       key,
		objectKey: objectKey,
		obj:       obj,
		md:        manifest.ObjectMetadata(),
		next: Fingerprint{
			SourceSize:  cand.Size,
			SourceMTime: cand.MTime,
			SourceHash:  sourceHash,
		},
	}
}

// keySpread is a cheap stable hash of a state key, used only to separate backoff wakeups.
func keySpread(k Key) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k.SourceID))
	_, _ = h.Write([]byte(k.NativePath))
	return h.Sum64()
}

// failAndBackOff parks a file: the decision is "parked" rather than "failed" because parked is what
// the counter and the heartbeat read. There is no attempt limit: giving up silently loses data.
func failAndBackOff(o Options, res *fileResult, key Key, fp Fingerprint, reason string) {
	res.outcome.Decision = auditlog.DecisionParked
	res.outcome.Reason = reason
	if o.DryRun {
		return
	}
	attempts := fp.Attempts + 1
	next := fp
	next.Attempts = attempts
	next.Parked = true
	next.LastError = reason
	// Spread by the file's own key so correlated failures do not all wake in the same second.
	next.BackoffUntil = o.Now().Add(backoffFor(attempts, keySpread(key)))
	res.intent = intent{kind: intentBackoff, key: key, fp: next, reason: reason}
}
