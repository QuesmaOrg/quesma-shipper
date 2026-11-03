package protocol_test

import (
	"encoding/json"
	"reflect"
	"testing"

	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
)

func TestAdditiveExclusionReductionFixtures(t *testing.T) {
	type source struct {
		protocol.SourceExclusions
		Exclude []string `json:"exclude,omitempty"`
	}
	type document struct {
		ConfigVersion int      `json:"config_version"`
		Sources       []source `json:"sources"`
	}
	var full, reduced document
	for name, target := range map[string]*document{"full": &full, "reduced": &reduced} {
		raw, err := protocol.FS.ReadFile("fixtures/v1/exclusion-reduction/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.ValidateConfigDocument(raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	scopes := make([]protocol.SourceExclusions, len(full.Sources))
	for i, source := range full.Sources {
		scopes[i] = source.SourceExclusions
	}
	for i, scope := range protocol.ReduceSourceExclusions(scopes) {
		full.Sources[i].SourceExclusions = scope
	}
	if !reflect.DeepEqual(full, reduced) {
		t.Fatalf("reduction = %+v, want %+v", full, reduced)
	}
}

func TestValidateExcludeAdd(t *testing.T) {
	for _, glob := range []string{"projects/**", "**/*.{png,pdf}", "a/[bc]?.jsonl", "private folder/**"} {
		if err := protocol.ValidateExcludeAdd(glob); err != nil {
			t.Errorf("%q: %v", glob, err)
		}
	}
	for _, glob := range []string{"", "/absolute", `\\server\share`, `C:\private`, "../private", "a/../b", `a\..\b`, "[", "a\\", "a\x00b", `projects\private\**`, `projects/private\**`, `projects\private/**`} {
		if err := protocol.ValidateExcludeAdd(glob); err == nil {
			t.Errorf("accepted %q", glob)
		}
		raw, err := json.Marshal(map[string]any{"config_version": 1, "sources": []map[string]any{{"id": "claude-code-transcripts", "exclude_add": []string{glob}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.ValidateConfigDocument(raw); err == nil {
			t.Errorf("schema accepted %q", glob)
		}
	}
}
