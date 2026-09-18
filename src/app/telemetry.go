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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
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

	Hostname      string             `json:"hostname,omitempty"`
	At            string             `json:"at,omitempty"`
	ClientVersion string             `json:"client_version,omitempty"`
	Consecutive   int                `json:"consecutive_failures,omitempty"`
	Faults        []telemetryFault   `json:"faults,omitempty"`
	LastCrash     *formats.LastCrash `json:"last_crash,omitempty"`
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
	endpoint := r.eff.TelemetryEndpoint
	if endpoint == "" || r.telemetry == nil {
		return
	}

	payload, err := json.Marshal(r.installHealth())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry: %v\n", err)
		return
	}
	err = r.telemetry.SubmitTelemetry(ctx, endpoint, uuid.NewString(), time.Now().UTC(), payload)
	switch {
	case err == nil:
	case errors.Is(err, controlplane.ErrTelemetryDisabled):
		// The organization turned it off, or this install was revoked. Either way the answer does
		// not change until configuration is loaded again, so stop asking for the rest of this run.
		r.eff.TelemetryEndpoint = ""
	default:
		// Rejected or undeliverable, and both are the same to this side: say so once and carry on
		// collecting, which is the thing that actually matters.
		fmt.Fprintf(os.Stderr, "warning: telemetry: %v\n", err)
	}
}

// installHealth is the event, built from the record the heartbeat also reads.
func (r *Runtime) installHealth() telemetryEvent {
	record := r.failureRecord()
	event := telemetryEvent{
		Event:         InstallHealthEvent,
		Hostname:      r.hostname,
		At:            time.Now().UTC().Format(time.RFC3339),
		ClientVersion: r.build.Version,
		Consecutive:   record.ConsecutiveFailures,
		LastCrash:     record.LastCrash,
	}
	for _, f := range record.Recent {
		event.Faults = append(event.Faults, telemetryFault{
			At: f.At, Kind: f.Kind, RunID: f.RunID, Message: telemetryMessage(f.Message),
		})
	}
	return event
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

// shortenTelemetryPaths keeps the last two segments of any absolute path, so a message says which
// file without saying where on the machine it lived.
func shortenTelemetryPaths(message string) string {
	var out strings.Builder
	for i := 0; i < len(message); {
		if message[i] != '/' {
			out.WriteByte(message[i])
			i++
			continue
		}
		end := i
		var segments []string
		for end < len(message) && message[end] == '/' {
			start := end + 1
			stop := start
			for stop < len(message) && !strings.ContainsRune("/ \t\n\"',;:)", rune(message[stop])) {
				stop++
			}
			if stop == start {
				break
			}
			segments = append(segments, message[start:stop])
			end = stop
		}
		if len(segments) < 3 {
			// Not a path worth shortening: written back as it was, including the slashes.
			out.WriteString(message[i:max(end, i+1)])
			i = max(end, i+1)
			continue
		}
		out.WriteString("…/" + strings.Join(segments[len(segments)-2:], "/"))
		i = end
	}
	return out.String()
}
