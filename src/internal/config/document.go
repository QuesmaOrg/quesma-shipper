package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Document is one config layer's contents. Every field is a pointer or a slice so absent stays distinguishable from zero.
type Document struct {
	// IssuedAt and Org are the served envelope: they describe the org's issuing event, so only the remote layer may carry them.
	IssuedAt *string `yaml:"issued_at"`
	Org      *string `yaml:"org"`

	ConfigVersion *int             `yaml:"config_version"`
	Mode          *Mode            `yaml:"mode"`
	Sources       []SourceOverride `yaml:"sources"`

	// Sink is read and discarded; the key survives so an older config.yaml with a `send:` block still parses.
	Sink map[string]any `yaml:"send"`

	Scrub          *Scrub              `yaml:"scrub"`
	Encryption     *Encryption         `yaml:"encryption"`
	MaxFilesPerRun *int                `yaml:"max_files_per_run"`
	DrainDeadline  *string             `yaml:"drain_deadline"`
	StateDir       *string             `yaml:"state_dir"`
	UploadTargets  []UploadTarget      `yaml:"upload_targets"`
	StructuralEx   map[string][]string `yaml:"structural_exempt"`

	// CrashReport is read and discarded; the key survives so an older config.yaml with a `crash_report:` block still parses.
	CrashReport map[string]any `yaml:"crash_report"`

	Autoupdate *Autoupdate `yaml:"autoupdate"`
}

// Autoupdate switches self-update at daemon startup; off is free from any layer, re-enabling is not.
type Autoupdate struct {
	Enabled *bool `yaml:"enabled"`
}

// Mode is the scheduling shape: Schedule is a Go duration such as "15m".
type Mode struct {
	Schedule *string `yaml:"schedule"`
}

// SourceOverride adjusts a compiled source. It cannot create one: an id absent from the catalog is refused.
type SourceOverride struct {
	ID           string   `yaml:"id"`
	Enabled      *bool    `yaml:"enabled"`
	Roots        []string `yaml:"roots"`
	Include      []string `yaml:"include"`
	Exclude      []string `yaml:"exclude"`
	MaxFileBytes *int64   `yaml:"max_file_bytes"`

	// Enrichers toggles a registered enricher; config can never attach one, that would be config installing code.
	// Not free for a DB-backed source: disabling cursor-transcript-join stops DB-side capture entirely.
	Enrichers map[string]bool `yaml:"enrichers"`
}

// UploadTarget pins one destination for a presigned upload ticket. Machine-owner only: the ticket carries its own authority.
type UploadTarget struct {
	// Origin is scheme://host[:port] and nothing else. No wildcards.
	Origin string `yaml:"origin"`

	// Addressing is "virtual-hosted" or "path-style"; an unknown form is refused rather than guessed at.
	Addressing string `yaml:"addressing"`

	// PathPrefix is empty for virtual-hosted and the fixed "/bucket" for path-style.
	PathPrefix string `yaml:"path_prefix"`

	// AllowLoopbackHTTP admits http:// for a loopback host. Development only.
	AllowLoopbackHTTP bool `yaml:"allow_loopback_http"`
}

// Scrub carries the detection-rule surface. Rule packs may only grow: additions make scrubbing stricter.
type Scrub struct {
	RulePacks []string `yaml:"rule_packs"`

	// SecretKeyNames adds field and env-var names whose value is a credential. Union like RulePacks.
	SecretKeyNames []string `yaml:"secret_key_names"`
}

// Encryption is the recipient surface: who can read what this install ships.
type Encryption struct {
	// AdditionalRecipients are age public keys sealed to alongside the install's own. Union: no layer may remove another's readers.
	AdditionalRecipients []string `yaml:"additional_recipients"`

	// IncludeInstallRecipient keeps the install's own key in the set. Absent means true; withholding needs another recipient.
	IncludeInstallRecipient *bool `yaml:"include_install_recipient"`
}

// ParseDocument decodes the machine owner's own file. Unknown fields are refused: a typo must not be a silent no-op.
func ParseDocument(raw []byte) (*Document, error) {
	return parseDocument(raw, true)
}

// ParseServedDocument decodes the org's served document, ignoring unknown fields; config_version stays a hard gate.
func ParseServedDocument(raw []byte) (*Document, error) {
	return parseDocument(raw, false)
}

func parseDocument(raw []byte, knownFields bool) (*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(knownFields)

	var d Document
	if err := dec.Decode(&d); err != nil {
		// io.EOF means an empty document: an empty layer, not a broken one.
		if errors.Is(err, io.EOF) {
			return &Document{}, nil
		}
		return nil, fmt.Errorf("config: parse document: %w", err)
	}
	return &d, nil
}

// LayeredDocument pairs a document with the layer it came from.
type LayeredDocument struct {
	Layer Layer
	Doc   *Document
}
