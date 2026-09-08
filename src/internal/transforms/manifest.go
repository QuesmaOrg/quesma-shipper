package transforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// ManifestVersion is the wire-contract version; downstream hard-errors on an unknown
// value rather than reading it partially.
const ManifestVersion = 1

// Manifest is the first tar entry of every mirror object, and the only place the native
// path exists on the wire. Field names and shapes mirror manifest.schema.json, which is
// the authority: it is validated on seal and on open, so the two cannot drift silently.
type Manifest struct {
	ManifestVersion int `json:"manifest_version"`

	OrganizationID string `json:"organization_id"`
	InstallID      string `json:"install_id"`

	SourceID      string `json:"source_id"`
	SourceFamily  string `json:"source_family,omitempty"`
	NativePath    string `json:"native_path"`
	Gather        string `json:"gather"`
	ArtifactClass string `json:"artifact_class"`

	// SourceHash covers the raw pre-redaction bytes, ShippedHash the archived ones. One
	// hash cannot do both: redaction changes bytes, so the shipped hash is not an identity.
	SourceHash  string `json:"source_hash"`
	ShippedHash string `json:"shipped_hash"`

	PayloadSize  int64      `json:"payload_size"`
	PayloadMTime *time.Time `json:"payload_mtime,omitempty"`

	AgentVersion string `json:"agent_version,omitempty"`
	ShapeSniff   string `json:"shape_sniff,omitempty"`

	Redaction *RedactionSummary `json:"redaction,omitempty"`

	Encryption *Encryption `json:"encryption,omitempty"`

	Client        Client `json:"client"`
	ConfigVersion int    `json:"config_version,omitempty"`
	ConfigExpired bool   `json:"config_expired,omitempty"`
	SealedAt      string `json:"sealed_at"`

	// RunID joins the object to the crash journal, audit entries and heartbeat of the
	// process that sealed it.
	RunID string `json:"run_id,omitempty"`

	Derived          bool          `json:"derived,omitempty"`
	Enricher         *EnricherRef  `json:"enricher,omitempty"`
	DerivedFrom      []string      `json:"derived_from,omitempty"`
	EnrichStatus     string        `json:"enrich_status,omitempty"`
	EnrichMismatches int           `json:"enrich_mismatches,omitempty"`
	DBProvenance     *DBProvenance `json:"db_provenance,omitempty"`

	// What an ok object could not enrich, without which a partial derived object is
	// byte-identical here to a complete one. Only line decode errors are loss: those lines
	// never reached the join (the torn final line is excluded).
	EnrichRepeats          int `json:"enrich_repeats,omitempty"`
	EnrichTail             int `json:"enrich_tail,omitempty"`
	EnrichAmbiguous        int `json:"enrich_ambiguous,omitempty"`
	EnrichLineDecodeErrors int `json:"enrich_line_decode_errors,omitempty"`
}

// RedactionSummary is rule-id and count granularity, never byte ranges: positions would
// fingerprint where and how large each secret was.
type RedactionSummary struct {
	Density       float64        `json:"density"`
	BytesRedacted int64          `json:"bytes_redacted,omitempty"`
	RuleHits      map[string]int `json:"rule_hits,omitempty"`
	ScanMode      string         `json:"scan_mode,omitempty"`
}

// Encryption records the recipients an object was encrypted to: public key IDs only.
type Encryption struct {
	Scheme          string   `json:"scheme"`
	RecipientKeyIDs []string `json:"recipient_key_ids"`
}

// Client identifies the build that sealed the object, as separate facts rather than one
// encoded version string, so grouping by build is a comparison and not a substring match.
// All optional but Version: a build with no VCS stamping has nothing truthful for Commit.
type Client struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Modified  bool   `json:"modified,omitempty"`
	GoVersion string `json:"go_version,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
}

type EnricherRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// DBProvenance records what an enricher read from a local agent database. The rows never
// ship, so this is the only account of where derived fields came from.
type DBProvenance struct {
	DBPath     string   `json:"db_path,omitempty"`
	ReadMethod string   `json:"read_method,omitempty"`
	Keyspaces  []string `json:"keyspaces,omitempty"`
	RowsRead   int      `json:"rows_read,omitempty"`
}

// Encode serializes a manifest and validates it against the schema, so one that would
// fail downstream validation never reaches a bucket.
func (m Manifest) Encode() ([]byte, error) {
	if m.ManifestVersion == 0 {
		m.ManifestVersion = ManifestVersion
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("seal: encode manifest: %w", err)
	}
	if err := validateManifestBytes(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DecodeManifest parses and validates a manifest.
func DecodeManifest(raw []byte) (Manifest, error) {
	if err := validateManifestBytes(raw); err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("seal: decode manifest: %w", err)
	}
	if m.ManifestVersion != ManifestVersion {
		return Manifest{}, fmt.Errorf("seal: manifest_version %d, this client speaks %d",
			m.ManifestVersion, ManifestVersion)
	}
	return m, nil
}

func validateManifestBytes(raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("seal: manifest is not valid JSON: %w", err)
	}
	if err := formats.Validate(formats.Manifest, doc); err != nil {
		return fmt.Errorf("seal: manifest does not satisfy its schema: %w", err)
	}
	return nil
}

// ObjectMetadata is the plaintext metadata attached to a PUT, duplicated out of the
// manifest so a consumer can dedupe with a HEAD instead of a ranged GET and a decrypt.
// The native path is deliberately absent: it travels only inside the age ciphertext.
func (m Manifest) ObjectMetadata() map[string]string {
	md := map[string]string{
		"manifest-version": fmt.Sprint(m.ManifestVersion),
		"source-id":        m.SourceID,
		"source-hash":      m.SourceHash,
		"shipped-hash":     m.ShippedHash,
		"artifact-class":   m.ArtifactClass,
	}
	if m.AgentVersion != "" {
		md["agent-version"] = m.AgentVersion
	}
	if m.ShapeSniff != "" {
		md["shape-sniff"] = m.ShapeSniff
	}
	if m.Derived {
		md["derived"] = "true"
	}
	if m.EnrichStatus != "" {
		md["enrich-status"] = m.EnrichStatus
	}
	return md
}
