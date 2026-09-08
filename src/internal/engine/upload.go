package engine

// The write path: sealed objects are authorized in bounded groups and PUT with short-lived
// tickets. The port is DECLARED here, never imported, so the loop cannot know a control plane
// exists. Nothing here is conditional: the local fingerprint document alone says what shipped.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
)

// maxBatchObjects and maxBatchBytes bound one authorization group: the protocol's declared limits
// and a memory bound at once. The concurrent pass may set a lower object bound (see pool.go).
const (
	maxBatchObjects = 32
	maxBatchBytes   = 64 << 20
)

// batcher accumulates sealed objects and sends a group once one more would breach either bound.
// maxObjects differs per path; the byte bound is the protocol's and is the same for both.
type batcher[T any] struct {
	maxObjects int
	send       func([]T)

	items []T
	bytes int64
}

func (b *batcher[T]) add(it T, size int64) {
	// Sent BEFORE the append: a group past the declared ciphertext limit is refused whole.
	// No object-count check here: the post-append flush keeps the count strictly below the cap.
	if len(b.items) > 0 && b.bytes+size > maxBatchBytes {
		b.flush()
	}
	b.items = append(b.items, it)
	b.bytes += size
	if len(b.items) >= b.maxObjects || b.bytes >= maxBatchBytes {
		b.flush()
	}
}

func (b *batcher[T]) flush() {
	if items := b.take(); len(items) > 0 {
		b.send(items)
	}
}

// take empties the accumulator without sending, for a run that has stopped uploading.
func (b *batcher[T]) take() []T {
	items := b.items
	b.items, b.bytes = nil, 0
	return items
}

func (b *batcher[T]) len() int { return len(b.items) }

// PreparedObject is one sealed object offered for authorization. Body is the exact ciphertext PUT,
// so its length is the size the ticket is signed for.
type PreparedObject struct {
	// ObjectID pairs the object with its ticket, unique within the group and meaningless outside.
	ObjectID string
	Key      string
	Body     []byte

	// SourceHash is the manifest's raw pre-redaction digest, never a checksum of Body.
	SourceHash string

	// Metadata holds unprefixed plaintext names, source-hash excluded: hashes and versions only,
	// never the path, which stays inside the ciphertext.
	Metadata map[string]string
}

// UploadPort is the write path: one call authorizes a bounded batch and PUTs each member.
// Implementations validate every ticket against the prepared key before sending bytes, and return
// one result per input object in input order; nil means that object's bytes are on the store.
type UploadPort interface {
	AuthorizeAndUpload(ctx context.Context, batch []PreparedObject) []error
}

var (
	// ErrUploadUnavailable is an authorization the control plane would not serve now. It must NEVER
	// wrap formats.ErrCredentialsRefused: this install is waiting, not revoked.
	ErrUploadUnavailable = errors.New("engine: upload authorization is unavailable")

	// ErrTicketExpired is the one verdict worth a second authorization inside a single run.
	ErrTicketExpired = errors.New("engine: upload ticket had expired")
)

// stagedUpload is a prepared object waiting for its authorization group; the accumulator that
// holds these lives on the loop goroutine alone, which is why it needs no lock.
type stagedUpload struct {
	res     fileResult
	pending *pendingPut
}

// sendBatch authorizes one bounded group and turns each verdict into a file result, on a batch
// goroutine: everything it touches arrived by value or by handover.
func (o Options) sendBatch(ctx context.Context, items []stagedUpload) []fileResult {
	batch := make([]PreparedObject, len(items))
	for i, it := range items {
		batch[i] = preparedFrom(i, it.pending.objectKey, it.pending.obj, it.pending.md)
	}
	outcomes := o.authorizeAndUpload(ctx, batch)
	res := make([]fileResult, len(items))
	for i, it := range items {
		res[i] = o.applyUploadOutcome(it, outcomes[i])
	}
	return res
}

// preparedFrom builds one descriptor from a sealed object and its manifest metadata. source-hash
// is lifted into its own field, and md is consumed here, so it is edited in place.
func preparedFrom(idx int, key string, body []byte, md map[string]string) PreparedObject {
	if md == nil {
		md = map[string]string{}
	}
	hash := md["source-hash"]
	delete(md, "source-hash")
	return PreparedObject{
		ObjectID:   strconv.Itoa(idx),
		Key:        key,
		Body:       body,
		SourceHash: hash,
		Metadata:   md,
	}
}

// authorizeAndUpload spends one group, with at most ONE reauthorization for expired tickets: a
// second expiry means the clock or the lease is wrong, and retrying only stalls everything else.
func (o Options) authorizeAndUpload(ctx context.Context, batch []PreparedObject) []error {
	outcomes := o.Upload.AuthorizeAndUpload(ctx, batch)
	if len(outcomes) != len(batch) {
		return sameOutcome(len(batch), fmt.Errorf(
			"engine: the upload port answered %d outcomes for %d objects", len(outcomes), len(batch)))
	}

	var expired []int
	for i, oc := range outcomes {
		if errors.Is(oc, ErrTicketExpired) {
			expired = append(expired, i)
		}
	}
	if len(expired) == 0 {
		return outcomes
	}

	again := make([]PreparedObject, len(expired))
	for i, at := range expired {
		again[i] = batch[at]
		again[i].ObjectID = strconv.Itoa(i)
	}
	second := o.Upload.AuthorizeAndUpload(ctx, again)
	if len(second) != len(again) {
		err := fmt.Errorf("engine: the upload port answered %d outcomes for %d reauthorized objects",
			len(second), len(again))
		for _, at := range expired {
			outcomes[at] = err
		}
		return outcomes
	}
	// Whatever the second attempt says is final, expiry included: there is no third.
	for i, at := range expired {
		outcomes[at] = second[i]
	}
	return outcomes
}

// sameOutcome is one verdict for a whole group, for port-contract violations with no detail.
func sameOutcome(n int, err error) []error {
	outcomes := make([]error, n)
	for i := range outcomes {
		outcomes[i] = err
	}
	return outcomes
}

// errorKind names an upload failure for a span. Never conflate the two: a refusal kills the
// install, while unavailability stops only this run's uploads.
func errorKind(err error) string {
	switch {
	case errors.Is(err, formats.ErrCredentialsRefused):
		return "credentials-refused"
	case errors.Is(err, ErrUploadUnavailable):
		return "authorize-unavailable"
	}
	return "upload"
}

// applyUploadOutcome is the verdict-to-decision map. No park branch: a failed upload persists
// nothing, so the next run re-prepares the same key and a backoff would only delay recovery.
func (o Options) applyUploadOutcome(it stagedUpload, oc error) (r fileResult) {
	r = it.res
	out := &r.outcome

	// No stored-size cross-check here: upload.ValidateTicket is what refuses a length mismatch.
	if oc != nil {
		out.Decision = auditlog.DecisionFailed
		out.Reason = oc.Error()
		out.Fatal = errors.Is(oc, formats.ErrCredentialsRefused)
		r.unavailable = errors.Is(oc, ErrUploadUnavailable)
		return r
	}

	// The fingerprint commits only what this machine read and sent; upload progress stays local.
	r.intent = intent{kind: intentShipped, key: it.pending.key, fp: it.pending.next}
	out.Decision = auditlog.DecisionShipped
	return r
}

// abandon is a sealed object the pass will not send. Nothing commits, so the next run prepares it
// again; callers reach here only with one of the two latches set.
func (p *sourcePass) abandon(it stagedUpload) (r fileResult) {
	r = it.res
	r.outcome.Decision = auditlog.DecisionFailed
	r.outcome.Fatal = p.fatal
	// The non-fatal case must be marked as a halt, so fold suppresses its duplicates too.
	r.unavailable = !p.fatal
	r.outcome.Reason = "not attempted: " + p.haltReason
	if p.fatal {
		r.outcome.Reason = "not attempted: this install's credentials were refused"
	}
	return r
}
