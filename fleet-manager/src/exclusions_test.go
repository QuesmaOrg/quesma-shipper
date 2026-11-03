package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
)

func TestAdditionsSavedBeforeEnrollmentNeverServeWeakerConfig(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	body := `{"sources":[{"id":"claude-code-transcripts","exclude_add":["projects/private/**"]},{"id":"codex-rollouts","enabled":true}]}`
	if w := putCollection(t, server, collectionPutBody(t, twoRecipients(t), body)); w.Code != http.StatusBadRequest {
		t.Fatalf("unreported source not acknowledged: %d %s", w.Code, w.Body)
	}
	if w := putCollectionAcknowledging(t, server, twoRecipients(t), body, "claude-code-transcripts", "codex-rollouts"); w.Code != http.StatusNoContent {
		t.Fatalf("save in empty fleet: %d %s", w.Code, w.Body)
	}
	before, _, _ := store.Get(context.Background(), configKey("acme"))
	key, id := enrollInstall(t, server, manager)
	var logs bytes.Buffer
	server.logger = log.New(&logs, "", 0)
	for _, catalog := range []*sourceCatalog{nil, testCatalog(), testCatalog()} {
		served := fetchCollection(t, server, key, id, catalog)
		source := served.Sources[0]
		if source.Enabled == nil || *source.Enabled || len(source.ExcludeAdd) != 0 {
			t.Fatalf("weaker response: %+v", source)
		}
		if served.Sources[1].Enabled == nil || !*served.Sources[1].Enabled {
			t.Fatal("unaffected source paused")
		}
	}
	if strings.Count(logs.String(), "paused source") != 1 {
		t.Fatalf("repeat pause logs: %s", logs.String())
	}
	supported := fetchCollection(t, server, key, id, testCatalog(protocol.ExcludeAddFeature))
	if !slices.Equal(supported.Sources[0].ExcludeAdd, []string{"projects/private/**"}) || supported.Sources[0].Enabled != nil {
		t.Fatalf("supported response altered: %+v", supported.Sources[0])
	}
	downgraded := fetchCollection(t, server, key, id, nil)
	if downgraded.Sources[0].Enabled == nil || *downgraded.Sources[0].Enabled {
		t.Fatal("downgrade used a previous catalog's capability")
	}
	malformed := fetchCollectionWith(t, server, key, id, json.RawMessage(`{"sources":[]}`))
	if malformed.Sources[0].Enabled == nil || *malformed.Sources[0].Enabled {
		t.Fatal("malformed catalog bypassed reduction")
	}
	after, _, _ := store.Get(context.Background(), configKey("acme"))
	if !bytes.Equal(before, after) {
		t.Fatal("serving reduced config rewrote stored additions")
	}
}

func TestFleetReductionMatchesProtocolFixtures(t *testing.T) {
	type document struct {
		ConfigVersion int `json:"config_version"`
		CollectionConfig
	}
	var full, reduced document
	for name, target := range map[string]*document{"full": &full, "reduced": &reduced} {
		raw, err := protocol.FS.ReadFile("fixtures/v1/exclusion-reduction/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err := strictDecode(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := collectionForBuild(&full.CollectionConfig, nil)
	if !reflect.DeepEqual(*got, reduced.CollectionConfig) {
		t.Fatalf("reduced %+v, want %+v", got, reduced.CollectionConfig)
	}
	if full.Sources[0].Enabled == nil || !*full.Sources[0].Enabled || len(full.Sources[0].ExcludeAdd) == 0 {
		t.Fatal("mutated full collection")
	}
}

func TestInvalidAdditiveExclusionsCannotChangeStoredCollection(t *testing.T) {
	server, store := testAdminServer(t)
	before, version, _ := store.Get(context.Background(), configKey("acme"))
	for _, pattern := range []string{"", "/private/**", "projects/../private/**", "projects/[", `C:\private\**`, `projects\private\**`, `projects/private\**`, `projects\private/**`} {
		body, _ := json.Marshal(map[string]any{"sources": []map[string]any{{"id": "claude-code-transcripts", "exclude_add": []string{pattern}}}})
		w := putCollectionAcknowledging(t, server, twoRecipients(t), string(body), "claude-code-transcripts")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d %s", pattern, w.Code, w.Body)
		}
		after, afterVersion, _ := store.Get(context.Background(), configKey("acme"))
		if !bytes.Equal(before, after) || version != afterVersion {
			t.Fatal("invalid addition changed config")
		}
	}
}
