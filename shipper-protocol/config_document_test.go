package protocol_test

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestConfigDocumentFixtures(t *testing.T) {
	paths, err := fs.Glob(protocol.FS, "fixtures/v1/config-document/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("config document fixtures: %v (%d files)", err, len(paths))
	}
	for _, assetPath := range paths {
		t.Run(path.Base(assetPath), func(t *testing.T) {
			raw, err := protocol.FS.ReadFile(assetPath)
			if err != nil {
				t.Fatal(err)
			}
			err = protocol.ValidateConfigDocument(raw)
			bad := strings.HasPrefix(path.Base(assetPath), "bad-")
			if bad && err == nil {
				t.Fatal("invalid authored document accepted")
			}
			if !bad && err != nil {
				t.Fatalf("valid authored document rejected: %v", err)
			}
		})
	}
}

func TestConfigDocumentValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"mode":{"schedule":"1m"},"drain_deadline":"1ns"}`} {
		if err := protocol.ValidateConfigDocument([]byte(raw)); err != nil {
			t.Errorf("rejected %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{`, `{} {}`, `null`,
		`{"max_files_per_run":1.5}`, `{"max_files_per_run":null}`,
		`{"drain_deadline":"0s"}`, `{"drain_deadline":"-1s"}`, `{"drain_deadline":"999999999999999999h"}`,
		`{"mode":{"schedule":"30s"}}`, `{"mode":{"schedule":"forever"}}`,
		`{"sources":"hello"}`, `{"sources":[{}]}`, `{"sources":[{"id":"example","enabled":"yes"}]}`,
		`{"unknown":true}`, `{"mode":{"unknown":true}}`,
		`{"send":{}}`, `{"send":{"sink":"s3","bucket":"example"}}`,
		`{"crash_report":{}}`, `{"crash_report":{"enabled":false}}`,
	} {
		if err := protocol.ValidateConfigDocument([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestConfigDocumentValidationErrorUsesSchemaID(t *testing.T) {
	err := protocol.ValidateConfigDocument([]byte(`{"drain_deadline":"forever"}`))
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("want a schema validation error, got %v", err)
	}
	const schemaID = "https://github.com/QuesmaOrg/shipper-protocol/schemas/config-document.schema.json"
	if validationErr.SchemaURL != schemaID+"#" {
		t.Errorf("schema URL = %q, want %q", validationErr.SchemaURL, schemaID+"#")
	}
	if message := err.Error(); strings.Contains(message, "file://") || !strings.Contains(message, schemaID) || !strings.Contains(message, "/drain_deadline") {
		t.Errorf("validation error must identify the embedded schema and invalid field without a filesystem URL: %s", message)
	}
}
