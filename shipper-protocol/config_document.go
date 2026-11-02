package protocol

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateConfigDocument checks a decoded served document represented as JSON.
// It validates new control-plane writes, including Go duration bounds; client
// resolution against compiled capabilities and local settings is still required.
func ValidateConfigDocument(rawJSON []byte) error {
	schema, err := configDocumentSchema()
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawJSON))
	if err != nil {
		return fmt.Errorf("config document: %w", err)
	}
	return schema.Validate(doc)
}

var configDocumentSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	const name = "schemas/config-document.schema.json"
	const schemaID = "https://github.com/QuesmaOrg/shipper-protocol/schemas/config-document.schema.json"
	raw, err := FS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{
		Name:     "go-positive-duration",
		Validate: durationAtLeast(time.Nanosecond),
	})
	compiler.RegisterFormat(&jsonschema.Format{
		Name:     "go-schedule-duration",
		Validate: durationAtLeast(time.Minute),
	})
	if err := compiler.AddResource(schemaID, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(schemaID)
})

func durationAtLeast(minimum time.Duration) func(any) error {
	return func(value any) error {
		s, ok := value.(string)
		if !ok {
			return nil // The schema's type keyword handles non-strings.
		}
		duration, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		if duration < minimum {
			return fmt.Errorf("must be at least %s", minimum)
		}
		return nil
	}
}
