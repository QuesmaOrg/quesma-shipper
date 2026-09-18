// Submitting how collection is going, so an operator learns about a failing machine without
// walking to it.
//
// The heartbeat carries the same record, sealed into the archive and opened with the organization's
// key. This leaves in the clear through the control plane, so it sends LESS, and what it sends is
// chosen rather than copied.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

// telemetrySubmitter is the one call this package needs from a control-plane client, as an
// interface so a test can watch what a tick would send.
type telemetrySubmitter interface {
	SubmitTelemetry(ctx context.Context, path, batchID string, issuedAt time.Time, payload json.RawMessage) error
}

// InstallHealthEvent is the one event this build produces, and its name is the contract with the
// collector: an unrecognised event is counted rather than rendered, so a rename goes silent.
const InstallHealthEvent = "install_health"

// maxTelemetryMessage bounds one fault's text. The collector truncates to the same length, so
// cutting here makes what is sent and what is stored the same thing.
const maxTelemetryMessage = 400

// telemetryEvent is what the collector reads. Field names match its own, and unknown fields are
// ignored at both ends, so this may grow without a release on the other side. Hostname is here
// because the body is forwarded verbatim: without it every alert names a machine by a UUID.
type telemetryEvent struct {
	Event string `json:"event"`

	Hostname      string           `json:"hostname,omitempty"`
	At            string           `json:"at,omitempty"`
	ClientVersion string           `json:"client_version,omitempty"`
	Consecutive   int              `json:"consecutive_failures,omitempty"`
	Faults        []telemetryFault `json:"faults,omitempty"`
	LastCrash     *telemetryCrash  `json:"last_crash,omitempty"`
}

// telemetryCrash is how the previous run died. Projected rather than embedding the record's own
// type, so a field added there for the sealed heartbeat does not start crossing in the clear.
// Phase is a closed vocabulary, "tick N".
type telemetryCrash struct {
	RunID       string `json:"run_id"`
	Phase       string `json:"phase"`
	Consecutive int    `json:"consecutive,omitempty"`
}

// telemetryFault is one failure, as the collector groups them. Kind comes from this package's
// closed set, so the far end groups on it without parsing prose.
type telemetryFault struct {
	At      string `json:"at"`
	Kind    string `json:"kind"`
	RunID   string `json:"run_id"`
	Message string `json:"message,omitempty"`
}

// SubmitTelemetry sends one install-health event, if this install's organization has a collector.
//
// Called by the tick loop AFTER the outcome is judged, and off the collection and upload path, so a
// slow collector can never delay shipping. Fail-open like the heartbeat beside it: telemetry must
// never become the problem it reports. Nothing retries -- the next tick carries its own batch id
// and the same bounded window, so one lost submission says nothing a later one does not repeat.
func (r *Runtime) SubmitTelemetry(ctx context.Context) {
	if r.eff.TelemetryEndpoint == "" || r.telemetry == nil || r.telemetryOff {
		return
	}

	// One instant for both stamps: two clock reads would make them disagree for no reason.
	now := time.Now().UTC()
	batch, payload, err := r.installHealth(now)
	if err == nil {
		err = r.telemetry.SubmitTelemetry(ctx, r.eff.TelemetryEndpoint, batch, now, payload)
	}
	switch {
	case err == nil:
	case errors.Is(err, controlplane.ErrTelemetryDisabled):
		// Turned off, or revoked: either way nothing changes until the next configuration load.
		r.telemetryOff = true
	default:
		// Rejected or undeliverable are the same here: say so and carry on collecting.
		fmt.Fprintf(os.Stderr, "warning: telemetry: %v\n", err)
	}
}

// installHealth is the event, built from the record the heartbeat also reads. The id and the body
// are returned together because an id minted at send time would identify the request rather than
// the event, which is what makes it useful when a resend arrives.
func (r *Runtime) installHealth(now time.Time) (batchID string, payload []byte, err error) {
	record := r.failureRecord()
	event := telemetryEvent{
		Event:         InstallHealthEvent,
		Hostname:      r.hostname,
		At:            now.Format(time.RFC3339),
		ClientVersion: r.build.Version,
		Consecutive:   record.ConsecutiveFailures,
	}
	if c := record.LastCrash; c != nil {
		event.LastCrash = &telemetryCrash{RunID: c.RunID, Phase: c.Phase, Consecutive: c.Consecutive}
	}
	event.Faults = make([]telemetryFault, 0, len(record.Recent))
	for _, f := range record.Recent {
		event.Faults = append(event.Faults, telemetryFault{
			At: f.At, Kind: f.Kind, RunID: f.RunID, Message: telemetryMessage(f.Message),
		})
	}
	payload, err = json.Marshal(event)
	if err != nil {
		return "", nil, fmt.Errorf("telemetry: %w", err)
	}
	return controlplane.NewBatchID(), payload, nil
}

// telemetryMessage is what a fault's text becomes on the way out. The username is already a
// placeholder; what is left in an absolute path is the directory it sat in, which on a working
// machine names a project or a client, and an alert has never needed that.
func telemetryMessage(message string) string {
	message = shortenTelemetryPaths(message)
	if len(message) <= maxTelemetryMessage {
		return message
	}
	// Cut from the FRONT: a wrapped error's cause is at the end. The marker counts against the
	// bound, and the cut is walked forward off any rune it landed inside.
	const marker = "…"
	tail := message[len(message)-(maxTelemetryMessage-len(marker)):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return marker + tail
}

// absolutePath is a run of three or more slash-prefixed segments. Three, because fewer is a route
// or a fraction rather than a place on this machine.
var absolutePath = regexp.MustCompile(`(?:/[^/ \t\n"',;:)]+){3,}`)

// shortenTelemetryPaths keeps the last two segments of any absolute path, so a message says which
// file without saying where on the machine it lived.
func shortenTelemetryPaths(message string) string {
	if !strings.Contains(message, "/") {
		return message
	}
	return absolutePath.ReplaceAllStringFunc(message, func(path string) string {
		segments := strings.Split(path, "/")
		return "…/" + strings.Join(segments[len(segments)-2:], "/")
	})
}
