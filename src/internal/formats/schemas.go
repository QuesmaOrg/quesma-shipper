package formats

// The shipper's wire contracts as JSON Schema documents: fingerprint-state (the local
// state/fingerprints.json), manifest (the first tar entry of every object) and source-spec (a
// catalog file's sources). Every object sets additionalProperties: false, so unknown fields fail.

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.schema.json
var FS embed.FS

// Names of the embedded schemas, as passed to Compile.
const (
	FingerprintState = "fingerprint-state.schema.json"
	Manifest         = "manifest.schema.json"
	SourceSpec       = "source-spec.schema.json"
)

// All lists every embedded schema, so tests can assert the set is complete.
var All = []string{FingerprintState, Manifest, SourceSpec}

var (
	mu       sync.Mutex
	compiled = map[string]*jsonschema.Schema{}
)

// Compile returns the compiled schema of the given name, cached after first use.
func Compile(name string) (*jsonschema.Schema, error) {
	mu.Lock()
	defer mu.Unlock()

	if s, ok := compiled[name]; ok {
		return s, nil
	}

	raw, err := FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("schemas: read %s: %w", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schemas: parse %s: %w", name, err)
	}

	c := jsonschema.NewCompiler()
	// Registered under the bare file name so callers pass a file name, not the document's $id.
	if err := c.AddResource(name, doc); err != nil {
		return nil, fmt.Errorf("schemas: add %s: %w", name, err)
	}
	s, err := c.Compile(name)
	if err != nil {
		return nil, fmt.Errorf("schemas: compile %s: %w", name, err)
	}

	compiled[name] = s
	return s, nil
}

// Validate checks an already-decoded document against the named schema.
func Validate(name string, doc any) error {
	s, err := Compile(name)
	if err != nil {
		return err
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("schemas: %s: %w", name, err)
	}
	return nil
}
