package engine_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOversizeHeartbeatContract(t *testing.T) {
	files := []formats.OversizeFile{{RelPath: "projects/-Users-alice-demo/session.jsonl", Size: 600000000, Limit: 536870912}}
	hb := engine.Build(engine.Input{
		OrganizationID: "acme", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		ClientVersion: "0.1.0-test", ConfigVersion: 1, Username: "alice",
		Now: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
		Report: formats.Report{Sources: []formats.SourceOutcome{{
			SourceID: "claude-code-transcripts", Family: "claude-code", Health: formats.Collected,
			Oversize: 1, OversizeFiles: files, Files: []formats.FileOutcome{{Decision: formats.DecisionShipped}},
		}}},
	})
	body, err := hb.Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.FS.ReadFile("fixtures/v1/heartbeat/oversize.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("heartbeat differs from contract fixture:\n%s", body)
	}
	if files[0].RelPath != "projects/-Users-alice-demo/session.jsonl" {
		t.Fatal("heartbeat modified the local report")
	}
	schemaRaw, err := protocol.FS.ReadFile("schemas/heartbeat.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaRaw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("heartbeat.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("heartbeat.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(got); err != nil {
		t.Fatal(err)
	}
}

func TestOversizeHeartbeatDistinguishesEmptyAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []formats.OversizeFile
		want  string
	}{
		{"scanned", []formats.OversizeFile{}, "[]"}, {"not scanned", nil, "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hb := engine.Build(engine.Input{Report: formats.Report{Sources: []formats.SourceOutcome{{OversizeFiles: tc.files}}}})
			body, err := hb.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, []byte(`"oversize_files": `+tc.want)) {
				t.Fatalf("wrong discovery coverage: %s", body)
			}
		})
	}
	var legacy engine.Heartbeat
	raw, err := protocol.FS.ReadFile("fixtures/v1/heartbeat/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Sources[0].OversizeFiles != nil || legacy.Sources[0].Oversize != 1 {
		t.Fatal("older heartbeats must retain their count with unknown details")
	}
}
