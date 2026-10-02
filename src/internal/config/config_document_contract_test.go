package config_test

import (
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	protocol "github.com/QuesmaOrg/shipper-protocol"
)

// The schema is an authoring profile, not a tightening of tolerant served reads.
// Exercise the same fixtures against the real compiled catalog and a populated
// fake home so a schema-valid document cannot silently outpace this client.
func TestConfigDocumentProtocolFixtures(t *testing.T) {
	// These are intentionally refused for new writes but accepted by this client.
	// Keep the exceptions named: a new bad fixture must fail client parsing or
	// resolution unless its compatibility distinction is explicitly reviewed.
	authoringOnly := map[string]string{
		"bad-count-fractional.json":       "YAML decoder converts numeric values into the integer field",
		"bad-count-negative.json":         "resolver historically accepts signed limits",
		"bad-issued-at.json":              "served issued_at is an uninterpreted envelope string",
		"bad-null.json":                   "null decodes as an empty layer",
		"bad-schedule-short.json":         "TickInterval clamps a short schedule after resolution",
		"bad-schedule-syntax.json":        "TickInterval falls back to the default after resolution",
		"bad-source-unknown-field.json":   "served parsing ignores unknown source fields",
		"bad-telemetry-network-path.json": "resolver checks a leading slash; authoring disallows network-path references",
		"bad-unknown-field.json":          "served parsing ignores unknown fields",
		"bad-upload-targets.json":         "an empty pin list has no effect",
	}
	paths, err := fs.Glob(protocol.FS, "fixtures/v1/config-document/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("served document fixtures: %v (%d files)", err, len(paths))
	}
	for _, assetPath := range paths {
		name := path.Base(assetPath)
		t.Run(name, func(t *testing.T) {
			raw, err := protocol.FS.ReadFile(assetPath)
			if err != nil {
				t.Fatal(err)
			}
			bad := strings.HasPrefix(name, "bad-")
			if schemaErr := protocol.ValidateConfigDocument(raw); (schemaErr != nil) != bad {
				t.Fatalf("authoring fixture classification changed: %v", schemaErr)
			}
			// JSON is a YAML subset, so these exact fixture bytes take the normal
			// served-document parser without an extra conversion or validation layer.
			doc, err := config.ParseServedDocument(raw)
			if err == nil {
				_, err = config.Resolve(baseInput(t, fakeHome(t), config.LayeredDocument{Layer: config.LayerRemote, Doc: doc}))
			}
			reason, compatibilityCase := authoringOnly[name]
			if !bad || compatibilityCase {
				if err != nil {
					t.Fatalf("client refused accepted fixture (%s): %v", reason, err)
				}
			} else if err == nil {
				t.Fatal("invalid fixture unexpectedly passed client parsing and resolution; document any authoring-only distinction explicitly")
			}
		})
		delete(authoringOnly, name)
	}
	if len(authoringOnly) != 0 {
		t.Fatalf("compatibility exceptions reference missing fixtures: %v", authoringOnly)
	}
}
