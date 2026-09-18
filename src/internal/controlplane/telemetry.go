// Submitting operational telemetry to the control plane.
//
// The control plane authenticates the install, checks whether its organization has a collector, and
// forwards the body under its own signature without reading it.
//
// Because the body is forwarded VERBATIM, anything the far end needs -- the machine's name, most of
// all -- has to be in here: nothing downstream can add to it.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TelemetryPreamble domain-separates this signature the way the v2 upload one is separated: the
// signed bytes are this prefix followed by the body, with no canonicalization. The path is inside
// it, so a signature cannot be replayed onto another route.
const TelemetryPreamble = "trajectory-shipper-telemetry-v1\nPOST\n" + TelemetryPath + "\n"

// TelemetryPath is the one route this signature is valid for. The control plane builds the same
// preamble from the same literal, so a served path that differs would be verified against something
// else -- a 401 with nothing to explain it. Refused locally instead.
const TelemetryPath = "/v1/telemetry"

// telemetrySchema is the envelope version; the payload carries its own event name.
const telemetrySchema = 1

// MaxTelemetryBody is what the control plane accepts. More is refused permanently, so this side
// checks first rather than spending a request to find out.
const MaxTelemetryBody = 1 << 20

// TelemetryRequest is one submission. IssuedAt is the caller's stamp and is the freshness the
// control plane checks.
type TelemetryRequest struct {
	Schema   int             `json:"schema"`
	BatchID  string          `json:"batch_id"`
	IssuedAt time.Time       `json:"issued_at"`
	Payload  json.RawMessage `json:"payload"`
}

// NewBatchID mints the identifier for one event, not for the request carrying it: the far end
// deduplicates on it, so a resend of the same event has to present the same id.
func NewBatchID() string { return uuid.NewString() }

// ErrTelemetryDisabled says this install's organization has no collector. Not a failure: the caller
// stops submitting until its next configuration load, because the answer cannot change before then.
var ErrTelemetryDisabled = errors.New("controlplane: telemetry is disabled for this organization")

// ErrTelemetryRejected is a submission this control plane will never accept -- a malformed
// envelope, a stale timestamp, an oversized body, or a collector refusing the payload.
var ErrTelemetryRejected = errors.New("controlplane: telemetry submission was rejected")

// ErrTelemetryUnavailable is a failure that may not recur: a rate limit, an unreachable collector,
// an unprovisioned deployment. Nothing here retries -- the next tick's submission is the retry.
//
// When bounded retry is added, this is where the server's Retry-After header has to arrive, and
// exchange will have to surface response headers to carry it.
var ErrTelemetryUnavailable = errors.New("controlplane: telemetry could not be delivered")

// SubmitTelemetry posts one event to path, taken from served configuration. A path rather than a
// URL: it resolves against the enrolled control-plane origin, so a served document cannot point
// telemetry at a third party. The batch id is the caller's and must survive a retry of the same
// event, which is what lets the far end recognise a duplicate.
func (c *Client) SubmitTelemetry(ctx context.Context, path, batchID string, issuedAt time.Time, payload json.RawMessage) error {
	if c.installID == "" {
		return ErrNotEnrolled
	}
	switch {
	case path == "":
		return ErrTelemetryDisabled
	case path != TelemetryPath:
		return fmt.Errorf("%w: the signature is only valid for %s, not %q",
			ErrTelemetryRejected, TelemetryPath, path)
	case batchID == "":
		return errors.New("controlplane: telemetry submission carries no batch_id")
	case issuedAt.IsZero():
		return errors.New("controlplane: telemetry submission carries no issued_at")
	case len(payload) == 0:
		return errors.New("controlplane: telemetry submission carries no payload")
	}

	body, err := json.Marshal(TelemetryRequest{
		Schema: telemetrySchema, BatchID: batchID, IssuedAt: issuedAt, Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("controlplane: encode telemetry submission: %w", err)
	}
	if len(body) > MaxTelemetryBody {
		// Refused for the same reason the server would refuse it, without spending the request.
		return fmt.Errorf("%w: %d bytes is past the %d limit", ErrTelemetryRejected, len(body), MaxTelemetryBody)
	}

	status, raw, err := c.exchange(ctx, path, TelemetryPreamble, body, true)
	if err != nil {
		return err
	}
	// Read only on a failure status: an accepted submission answers with nothing worth allocating.
	reason := func() string { return truncate(strings.TrimSpace(string(raw)), 200) }

	switch {
	case status/100 == 2:
		return nil
	case status == http.StatusForbidden:
		// Disabled for this organization, or this install was revoked.
		return ErrTelemetryDisabled
	case status == http.StatusBadRequest, status == http.StatusConflict,
		status == http.StatusRequestEntityTooLarge, status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w (HTTP %d): %s", ErrTelemetryRejected, status, reason())
	default:
		return fmt.Errorf("%w (HTTP %d): %s", ErrTelemetryUnavailable, status, reason())
	}
}
