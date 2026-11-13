package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Large JSONL keeps the same snapshot object and commit semantics, with disk replacing whole-file buffers.
func (o Options) prepareStream(ctx context.Context, job fileJob, src sources.Resolved, disc sources.Discovery, res fileResult) (fileResult, *pendingPut) {
	cand, fp := job.cand, job.fp
	key := KeyOf(src.ID, cand)
	fail := func(err error) (fileResult, *pendingPut) {
		failAndBackOff(o, &res, key, fp, err.Error())
		return res, nil
	}
	// A timestamp-only change should cost a hash, not another full scrub and spool.
	if job.seen && fp.SourceHash != "" && fp.SourceSize == cand.Size && o.policySatisfied(fp) {
		hash, size, err := hashStream(ctx, cand)
		if err != nil {
			return fail(err)
		}
		if hash == fp.SourceHash {
			res.outcome.BytesIn = size
			return o.unchangedStream(key, cand, fp, res), nil
		}
	}
	if o.scrubErr != nil {
		return fail(o.scrubErr)
	}
	stream, err := cand.Open(ctx)
	if err != nil {
		return fail(err)
	}
	defer stream.Close()
	plain, err := platform.NewSpool()
	if err != nil {
		return fail(fmt.Errorf("create scrub spool: %w", err))
	}
	defer plain.Close()
	scrubbed, sourceHash, err := o.scrub.ScrubTo(ctx, plain, stream, transforms.Hint{Family: src.Family, JSONL: true})
	if err != nil {
		return fail(fmt.Errorf("stream scrub failed closed: %w", err))
	}
	out := &res.outcome
	out.BytesIn = int64(scrubbed.BytesTotal)
	if o.policySatisfied(fp) && job.seen && sourceHash == fp.SourceHash {
		return o.unchangedStream(key, cand, fp, res), nil
	}
	out.Density, out.RuleHits = scrubbed.Density(), scrubbed.RuleHits
	if job.seen && cand.Size < fp.SourceSize && cand.Path == observedPath(key, fp) {
		out.Reason = "file shrank: truncation or rewrite"
	}
	objectKey, err := o.mirrorKey(src.ID, cmp.Or(cand.Identity, cand.RelPath))
	if err != nil {
		return fail(err)
	}
	out.ObjectKey = objectKey
	cipher, err := platform.NewSpool()
	if err != nil {
		return fail(fmt.Errorf("create encrypted spool: %w", err))
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = cipher.Close()
		}
	}()
	limited := &objectWriter{ctx: ctx, w: cipher}
	manifest := o.manifestFor(src, cand, disc, sourceHash, stream.MTime, scrubbed)
	sealed, err := transforms.SealTo(limited, manifest, scrubbed.Out, o.Recipients)
	if err != nil {
		return fail(err)
	}
	out.BytesOut = limited.size
	if o.DryRun {
		out.Decision = auditlog.DecisionShipped
		if out.Reason == "" {
			out.Reason = "would ship (preview)"
		}
		return res, nil
	}
	transferred = true
	return res, &pendingPut{key: key, objectKey: objectKey, spool: cipher, size: limited.size, md: sealed.ObjectMetadata(),
		next: Fingerprint{NativePath: cand.Path, SourceSize: cand.Size, SourceMTime: cand.MTime, SourceHash: sourceHash, SealPolicy: o.sealPolicy}}
}

func hashStream(ctx context.Context, cand sources.Candidate) (string, int64, error) {
	stream, err := cand.Open(ctx)
	if err != nil {
		return "", 0, err
	}
	defer stream.Close()
	hash := sha256.New()
	size, err := io.CopyBuffer(hash, stream, make([]byte, 256<<10))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (o Options) unchangedStream(key Key, cand sources.Candidate, fp Fingerprint, res fileResult) fileResult {
	res.outcome.Decision, res.outcome.Reason = auditlog.DecisionUnchanged, "content hash unchanged"
	if !o.DryRun {
		fp.NativePath, fp.SourceSize, fp.SourceMTime = cand.Path, cand.Size, cand.MTime
		fp.Parked, fp.LastError, fp.BackoffUntil, fp.Attempts = false, "", time.Time{}, 0
		res.intent = intent{kind: intentRefresh, key: key, fp: fp}
	}
	return res
}

// The existing upload contract permits at most 1 GiB of ciphertext per object.
const maxStreamObjectBytes = 1 << 30

type objectWriter struct {
	ctx  context.Context
	w    io.Writer
	size int64
}

func (w *objectWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > maxStreamObjectBytes-w.size {
		return 0, fmt.Errorf("encrypted object exceeds the %d-byte upload limit", maxStreamObjectBytes)
	}
	n, err := w.w.Write(p)
	w.size += int64(n)
	return n, err
}
