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
			if (err != nil) != bad {
				t.Fatalf("client disagrees with fixture: %v", err)
			}
		})
	}
}
