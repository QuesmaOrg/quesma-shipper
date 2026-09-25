// Health is what an install reported going wrong, as told to this service by whoever can read the
// archive. Fleet manager cannot read a heartbeat itself: the object is age-encrypted and this
// service holds recipients, never identities. So the component that legitimately holds the identity
// reports a plaintext summary, and this stores and serves it.
//
// Disposable, like the seen records and for the same reason: the hot path must never rewrite a
// security record, and nothing here is authoritative about anything.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// maxFaults bounds one report. The shipper's own log holds twenty, but every write leaves a
// permanent noncurrent version in a versioned bucket, and the newest few are what a table shows.
const maxFaults = 5

// maxFaultMessage is longer than the other client facts on purpose: a message's cause is at the end
// of it, and the shorter bound removes exactly the part worth reading.
const maxFaultMessage = 400

// Fault is one thing that went wrong, as the shipper classified it.
type Fault struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	At      string `json:"at,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

// HealthReport is what a caller may say: the shipper's own count and its faults, and nothing else.
// It is a separate type from the stored record so that the fields this service owns -- when the
// report arrived, which install it is about -- cannot arrive in a request body at all. The decoder
// is strict, so a body naming one of them is refused rather than quietly ignored.
type HealthReport struct {
	// Consecutive is the shipper's own count, which a success resets. Zero with faults present is an
	// install that had a bad run and recovered.
	Consecutive int `json:"consecutive_failures,omitempty"`

	Faults []Fault `json:"faults,omitempty"`
}

// HealthRecord is one install's last report, as stored. Absence is meaningful and distinct from an
// empty fault list: "nobody has told us" is not "nothing is wrong", and rendering them alike turns
// an unwatched install into a healthy-looking one.
type HealthRecord struct {
	Schema    int    `json:"schema"`
	InstallID string `json:"install_id"`

	// ReportedAt is when THIS service received the report, on its own clock, and never the caller's.
	// The UI decides "stale" from it; a reporter with a skewed clock, or a body carrying a future
	// time, would otherwise keep obsolete health looking current indefinitely.
	ReportedAt time.Time `json:"reported_at"`

	Consecutive int     `json:"consecutive_failures,omitempty"`
	Faults      []Fault `json:"faults,omitempty"`
}

// ErrNoFaultKind is a report with nothing usable in it. Accepting one would store a record that
// renders as "reported, healthy".
var ErrNoFaultKind = errors.New("every fault needs a kind")

// ValidateHealthReport checks what a caller sent before any of it is stored.
func ValidateHealthReport(report HealthReport) error {
	for i, f := range report.Faults {
		if clientFact(f.Kind) == "" {
			return fmt.Errorf("fault %d: %w", i, ErrNoFaultKind)
		}
	}
	return nil
}

// ReportHealth stores one install's report. Unconditional, like Touch: a lost concurrent report
// costs the next one nothing, and conflict retries would be a poor trade for a disposable record.
func (m *Manager) ReportHealth(ctx context.Context, installID string, report HealthReport) error {
	// Refused for an install this organization does not have, so the health prefix cannot be used
	// as arbitrary storage.
	if _, err := m.LoadActiveInstall(ctx, installID); err != nil {
		return err
	}

	rec := HealthRecord{
		Schema:      schemaVersion,
		InstallID:   installID,
		ReportedAt:  m.time(),
		Consecutive: report.Consecutive,
		Faults:      boundFaults(report.Faults),
	}

	encoded, err := encodeRecord(rec)
	if err != nil {
		return err
	}
	return m.store.Put(ctx, healthKey(m.org, installID), encoded)
}

// boundFaults keeps the newest few, every field bounded: this text was chosen by a machine nobody
// here administers and it lands in an administrator's browser.
func boundFaults(faults []Fault) []Fault {
	if n := len(faults); n > maxFaults {
		faults = faults[n-maxFaults:]
	}
	out := make([]Fault, 0, len(faults))
	for _, f := range faults {
		f.Kind = clientFact(f.Kind)
		f.At = clientFact(f.At)
		f.RunID = clientFact(f.RunID)
		f.Message = boundedText(f.Message, maxFaultMessage)
		if f.Kind == "" {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ListHealth drops unusable reports so the rest of an install table still renders.
func (m *Manager) ListHealth(ctx context.Context) ([]HealthRecord, error) {
	objects, err := m.store.List(ctx, controlPrefix(m.org)+"health/")
	if err != nil {
		return nil, err
	}
	return readRecords(ctx, m.store, objectKeys(objects), func(rec HealthRecord, key string) bool {
		return rec.Schema == schemaVersion && rec.InstallID == recordIDFromKey(key)
	}), nil
}
