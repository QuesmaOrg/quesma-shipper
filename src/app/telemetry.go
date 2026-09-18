// Submitting how collection is going, so an operator learns about a failing machine without
// walking to it.
//
// Everything here is already assembled for the heartbeat: the same failure record, the same
// consecutive counter, the same crash. The difference is where it goes and who can read it. A
// heartbeat is sealed into the archive and opened by whoever holds the organization's key; this
// leaves in the clear, through the control plane, to a collector run by whoever operates the fleet.
// So this file sends LESS than the heartbeat does, and what it sends is chosen rather than copied.
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

// telemetrySubmitter is the one call this package needs from a control-plane client. An interface
// so the runtime does not grow a dependency on the whole client, and so a test can watch what a
// tick would send.
type telemetrySubmitter interface {
	SubmitTelemetry(ctx context.Context, path, batchID string, issuedAt time.Time, payload json.RawMessage) error
}

// InstallHealthEvent is the one event this build produces. The name is part of the contract with
// the collector: an event it does not recognise is accepted and counted rather than rendered, so a
// rename here goes silent rather than loud.
const InstallHealthEvent = "install_health"

// maxTelemetryMessage bounds one fault's text. The collector truncates to the same length, so
// cutting here makes what is sent and what is stored the same thing rather than leaving the far end
// to decide where the sentence ends.
const maxTelemetryMessage = 400

// telemetryEvent is what the collector reads. Field names match its own, and unknown fields are
// ignored at both ends, so this may grow without a release on the other side.
//
// Hostname is here because nothing downstream can add it: the control plane forwards this body
// verbatim as the bytes it signs, so a name added in transit would break its signature. Without it
// every alert names a machine by a UUID nobody recognises.
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
// type, so that a field added to that type for the heartbeat's benefit -- which is sealed -- does
// not silently start crossing in the clear too. Phase comes from a closed vocabulary, "tick N".
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
// Called by the tick loop AFTER the outcome is judged, not from inside a flush: the record it
// reports is written by judging, and the contract asks that this stays off the collection and
// upload path so a slow collector can never delay shipping.
//
// Fail-open by contract, like the heartbeat beside it: telemetry is how someone hears about a
// problem, and it must never become one. Nothing retries, because retry is re-run everywhere in
// this program -- the next tick carries its own batch id and the same bounded window of failures,
// so one lost submission costs nothing a later one does not say again.
func (r *Runtime) SubmitTelemetry(ctx context.Context) {
	if r.eff.TelemetryEndpoint == "" || r.telemetry == nil || r.telemetryOff {
		return
	}

	// One instant for both stamps: the envelope's and the event's are meant to describe the same
	// moment, and two clock reads make them disagree for no reason.
	now := time.Now().UTC()
	batch, payload, err := r.installHealth(now)
	if err == nil {
		err = r.telemetry.SubmitTelemetry(ctx, r.eff.TelemetryEndpoint, batch, now, payload)
	}
	switch {
	case err == nil:
	case errors.Is(err, controlplane.ErrTelemetryDisabled):
		// The organization turned it off, or this install was revoked. Either way the answer does
		// not change until configuration is loaded again, which is the next process.
		r.telemetryOff = true
	default:
		// Rejected or undeliverable, and both are the same to this side: say so once and carry on
		// collecting, which is the thing that actually matters.
		fmt.Fprintf(os.Stderr, "warning: telemetry: %v\n", err)
	}
}

// installHealth is the event, built from the record the heartbeat also reads, with the identity the
// far end deduplicates on. The id and the body are returned together because they are one thing: an
// id minted at the moment of sending would identify the request rather than the event, which is the
// one property that makes it useful when a resend arrives.
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

// telemetryMessage is what a fault's text becomes on the way out.
//
// The username is already a placeholder by the time a message is recorded. What is left in an
// absolute path is the directory it sat in, which on a working machine names a project or a client,
// and that is a detail an alert has never needed: the last two segments say which file, and the
// cause is at the end of the line anyway.
func telemetryMessage(message string) string {
	message = shortenTelemetryPaths(message)
	if len(message) <= maxTelemetryMessage {
		return message
	}
	// Cut from the FRONT: an error's cause is at the end of a wrapped chain, so the tail is the part
	// worth keeping. The marker counts against the bound like anything else, and the cut is walked
	// forward off any rune it landed inside.
	const marker = "…"
	tail := message[len(message)-(maxTelemetryMessage-len(marker)):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return marker + tail
}

// absolutePath is a run of three or more slash-prefixed segments. Three, because fewer is a route
// or a fraction rather than somewhere on this machine, and the delimiters are what ends a path when
// it sits inside a sentence.
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
