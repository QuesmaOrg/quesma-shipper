// Heartbeat construction. Discovery health is NOT upload tracking: per source, no cursor offsets,
// no object keys, no statement about what was uploaded. It answers "is this source still
// findable", and it needs nothing configured, so it carries while any backend is unreachable.
package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// Name is the heartbeat object's name under the install's state prefix.
const Name = "heartbeat.json"

// Heartbeat is the whole document: every field is about the collector, none about a payload.
type Heartbeat struct {
	HeartbeatVersion int    `json:"heartbeat_version"`
	At               string `json:"at"`

	OrganizationID string `json:"organization_id"`
	InstallID      string `json:"install_id"`

	ClientVersion string `json:"client_version"`
	ConfigVersion int    `json:"config_version"`

	// ConfigExpired says collection continued under a cached config past its expiry, since expiry
	// fails toward collecting.
	ConfigExpired bool `json:"config_expired,omitempty"`

	// RunID ties this heartbeat to the crash journal and audit entries the same process wrote.
	RunID string `json:"run_id,omitempty"`

	// Embedded, so the fields stay at the top level of the document where readers already expect
	// last_crash. A heartbeat only ships from a run that got far enough to upload, so everything
	// here describes a PAST failure by construction.
	formats.FailureRecord

	Sources []SourceHealth `json:"sources"`
}

// SourceHealth is one source's discovery state.
type SourceHealth struct {
	SourceID string `json:"source_id"`
	Family   string `json:"family,omitempty"`

	// State distinguishes agent_absent from root_present_no_match: expected silence versus drift.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`

	Sniff        string `json:"shape_sniff,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`

	// FilesSeen and BytesRead describe volume, not content.
	FilesSeen int `json:"files_seen"`
	Shipped   int `json:"shipped"`
	Unchanged int `json:"unchanged"`
	Parked    int `json:"parked"`
	Failed    int `json:"failed"`

	// Oversize counts files the size cap excluded: never collected, then reaped by the agent.
	Oversize int `json:"oversize,omitempty"`

	// Unreadable counts paths discovery could not look at, which this install will never ship.
	Unreadable int `json:"unreadable,omitempty"`

	// EnrichMismatch is a TOP-LEVEL alarm: the derived view is the only carrier of the DB-side
	// fields, so a mismatch means they are lost until a release fixes the join.
	Enriched       int `json:"enriched,omitempty"`
	EnrichSkipped  int `json:"enrich_skipped,omitempty"`
	EnrichMismatch int `json:"enrich_mismatch,omitempty"`
	EnrichErrors   int `json:"enrich_errors,omitempty"`

	// Enricher names the code that produced this source's derived objects.
	Enricher string `json:"enricher,omitempty"`

	// RedactionDensity is the mean across this source's files; a fleet-wide spike means rule drift.
	RedactionDensity float64 `json:"redaction_density,omitempty"`

	LastCollectedAt string `json:"last_collected_at,omitempty"`
}

// Input is what a heartbeat is built from.
type Input struct {
	OrganizationID string
	InstallID      string
	ClientVersion  string
	ConfigVersion  int
	ConfigExpired  bool
	RunID          string
	Report         formats.Report
	Now            time.Time

	// FailureRecord is read back from disk, not built from this run: the run assembling a
	// heartbeat is by definition one that got far enough to upload.
	FailureRecord formats.FailureRecord
}

// Build turns one run's report into a heartbeat.
func Build(in Input) Heartbeat {
	hb := Heartbeat{
		HeartbeatVersion: 1,
		At:               in.Now.UTC().Format(time.RFC3339),
		OrganizationID:   in.OrganizationID,
		InstallID:        in.InstallID,
		ClientVersion:    in.ClientVersion,
		ConfigVersion:    in.ConfigVersion,
		ConfigExpired:    in.ConfigExpired,
		RunID:            in.RunID,
		FailureRecord:    in.FailureRecord,
		Sources:          []SourceHealth{},
	}

	for _, s := range in.Report.Sources {
		sh := SourceHealth{
			SourceID:     s.SourceID,
			Family:       s.Family,
			State:        string(s.Health),
			Reason:       s.Reason,
			Sniff:        string(s.Sniff),
			AgentVersion: s.AgentVersion,
			FilesSeen:    len(s.Files),
			Oversize:     s.Oversize,
			Unreadable:   s.Unreadable,

			Enriched:       s.Enriched,
			EnrichSkipped:  s.EnrichSkipped,
			EnrichMismatch: s.EnrichMismatch,
			EnrichErrors:   s.EnrichErrors,
		}
		if s.EnricherID != "" {
			sh.Enricher = fmt.Sprintf("%s@%d", s.EnricherID, s.EnricherVersion)
		}

		var densitySum float64
		var densityCount int
		for _, f := range s.Files {
			switch f.Decision {
			case "shipped":
				sh.Shipped++
			case "unchanged":
				sh.Unchanged++
			case "parked":
				sh.Parked++
			case "failed":
				sh.Failed++
			}
			if f.Density > 0 {
				densitySum += f.Density
				densityCount++
			}
		}
		if densityCount > 0 {
			sh.RedactionDensity = densitySum / float64(densityCount)
		}
		if sh.Shipped > 0 {
			sh.LastCollectedAt = in.Now.UTC().Format(time.RFC3339)
		}
		hb.Sources = append(hb.Sources, sh)
	}
	return hb
}

// Encode serializes the heartbeat.
func (h Heartbeat) Encode() ([]byte, error) {
	body, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("health: encode heartbeat: %w", err)
	}
	return append(body, '\n'), nil
}
