package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	protocol "github.com/QuesmaOrg/shipper-protocol"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// CollectionConfig is the administrator-owned subset of a served document.
type CollectionConfig struct {
	Mode           *CollectionMode    `json:"mode,omitempty" yaml:"mode,omitempty"`
	Sources        []CollectionSource `json:"sources,omitempty" yaml:"sources,omitempty"`
	Scrub          *CollectionScrub   `json:"scrub,omitempty" yaml:"scrub,omitempty"`
	MaxFilesPerRun *int               `json:"max_files_per_run,omitempty" yaml:"max_files_per_run,omitempty"`
	DrainDeadline  *string            `json:"drain_deadline,omitempty" yaml:"drain_deadline,omitempty"`
}

type CollectionMode struct {
	Schedule *string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
}
type CollectionSource struct {
	ID           string          `json:"id" yaml:"id"`
	Enabled      *bool           `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Roots        []string        `json:"roots,omitempty" yaml:"roots,omitempty"`
	Include      []string        `json:"include,omitempty" yaml:"include,omitempty"`
	Exclude      []string        `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	ExcludeAdd   []string        `json:"exclude_add,omitempty" yaml:"exclude_add,omitempty"`
	MaxFileBytes *int64          `json:"max_file_bytes,omitempty" yaml:"max_file_bytes,omitempty"`
	Enrichers    map[string]bool `json:"enrichers,omitempty" yaml:"enrichers,omitempty"`
}
type CollectionScrub struct {
	RulePacks      []string `json:"rule_packs,omitempty" yaml:"rule_packs,omitempty"`
	SecretKeyNames []string `json:"secret_key_names,omitempty" yaml:"secret_key_names,omitempty"`
}

// validateCollectionJSON checks a collection as the administrator sent it, before decoding drops
// what the schema refuses but a Go struct cannot see, such as an explicit null.
func validateCollectionJSON(raw []byte) error {
	if err := protocol.ValidateConfigDocument(raw); err != nil {
		return schemaError("collection", err)
	}
	return nil
}

// schemaError names the offending field instead of the validator's schema URL, so the admin knows
// what to correct.
func schemaError(prefix string, err error) error {
	var invalid *jsonschema.ValidationError
	if !errors.As(err, &invalid) {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	var problems []string
	for _, line := range strings.Split(err.Error(), "\n")[1:] {
		line = strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if at, rest, found := strings.Cut(strings.TrimPrefix(line, "at '"), "': "); found && strings.HasPrefix(line, "at '") {
			if field := strings.ReplaceAll(strings.TrimPrefix(at, "/"), "/", "."); field != "" {
				rest = field + ": " + rest
			}
			line = rest
		}
		if line != "" {
			problems = append(problems, line)
		}
	}
	if len(problems) == 0 {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return fmt.Errorf("%s: %s", prefix, strings.Join(problems, "; "))
}

func normalizeCollection(cfg *FleetConfig) error { return normalizeCollectionMode(cfg, true) }

// Existing YAML keeps the client's tolerant read semantics; the authoring schema gates writes.
func normalizeCollectionMode(cfg *FleetConfig, strict bool) error {
	if cfg.AuthoredYAML != "" && cfg.Collection != nil {
		return errors.New("provide collection or authored_yaml, not both")
	}
	collection := cfg.Collection
	if collection == nil {
		collection = &CollectionConfig{}
		if strict && strings.TrimSpace(cfg.AuthoredYAML) != "" {
			var document any
			if err := yaml.Unmarshal([]byte(cfg.AuthoredYAML), &document); err != nil {
				return fmt.Errorf("authored_yaml migration: %w", err)
			}
			if document == nil {
				document = map[string]any{} // only comments: no settings, as the read path takes it
			}
			raw, err := json.Marshal(document)
			if err != nil {
				return fmt.Errorf("authored_yaml migration: %w", err)
			}
			if err := protocol.ValidateConfigDocument(raw); err != nil {
				return schemaError("authored_yaml", err)
			}
		}
		dec := yaml.NewDecoder(bytes.NewBufferString(cfg.AuthoredYAML))
		// A key no field holds is one the shipper ignores too: dropped when served and on the next write.
		dec.KnownFields(strict)
		if err := dec.Decode(collection); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("authored_yaml migration: %w", err)
		}
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
			return errors.New("authored_yaml must contain one YAML document")
		}
	}
	raw, err := json.Marshal(struct {
		ConfigVersion int `json:"config_version"`
		*CollectionConfig
	}{1, collection})
	if err != nil {
		return err
	}
	if strict {
		if err := protocol.ValidateConfigDocument(raw); err != nil {
			return schemaError("collection", err)
		}
	}
	cfg.Collection, cfg.AuthoredYAML, cfg.CollectionError = collection, "", ""
	return nil
}
