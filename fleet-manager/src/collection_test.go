package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCollectionRejectsInvalidWritesWithoutChangingStoredConfig(t *testing.T) {
	server, store := testAdminServer(t)
	original, version, _ := store.Get(context.Background(), configKey("acme"))
	for _, collection := range []string{
		`{"drain_deadline":"forever"}`, `{"max_files_per_run":-1}`,
		`{"mode":{"schedule":"5s"}}`, `{"sources":[{"id":"claude-code","enabled":"yes"}]}`,
		`{"sources":[{"id":"claude-code","bogus":true}]}`, `{"state_dir":"/tmp/other"}`,
		`{"scrub":{"enabled":false}}`, `{"sources":[null]}`,
		`null`, `{"max_files_per_run":null}`, `{"sources":[{"id":"cursor-transcripts","enrichers":{"cursor-transcript-join":null}}]}`,
	} {
		t.Run(collection, func(t *testing.T) {
			body := map[string]any{"age_recipients": twoRecipients(t), "collection": json.RawMessage(collection)}
			raw, _ := json.Marshal(body)
			req := adminRequest("PUT", "/v1/admin/orgs/acme/config", string(raw), testAdminCredential)
			req.Header.Set("If-Match", encodeETag(version))
			w := serveAdmin(t, server, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d: %s", w.Code, w.Body)
			}
			got, gotVersion, _ := store.Get(context.Background(), configKey("acme"))
			if string(got) != string(original) || gotVersion != version {
				t.Fatal("rejected write changed stored config")
			}
		})
	}
}

func TestLegacyCollectionMigrationPreservesSettingsAndStoredBytes(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	cfg, version, _ := manager.LoadConfig(context.Background())
	cfg.Collection = nil
	cfg.AuthoredYAML = "mode: {schedule: 2m}\nmax_files_per_run: 17\ndrain_deadline: 90s\nsources:\n  - id: claude-code\n    enabled: false\n    roots: ['~/logs']\n    include: ['*.jsonl']\n    exclude: ['private/**']\n    max_file_bytes: 10000\nscrub:\n  secret_key_names: [my_secret]\n"
	if err := replaceRecord(context.Background(), store, configKey("acme"), version, cfg); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.Get(context.Background(), configKey("acme"))
	loaded, version, err := manager.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AuthoredYAML != "" || loaded.Collection == nil || *loaded.Collection.MaxFilesPerRun != 17 || loaded.Collection.Sources[0].Roots[0] != "~/logs" {
		t.Fatalf("migration lost settings: %+v", loaded)
	}
	after, _, _ := store.Get(context.Background(), configKey("acme"))
	if string(before) != string(after) {
		t.Fatal("read mutated durable config")
	}
	var served map[string]any
	if err := yaml.Unmarshal([]byte(renderConfig(loaded)), &served); err != nil {
		t.Fatal(err)
	}
	if served["drain_deadline"] != "90s" {
		t.Fatalf("served settings lost: %v", served)
	}
	if err := manager.ApplyConfig(context.Background(), loaded, version); err != nil {
		t.Fatal(err)
	}
	after, _, _ = store.Get(context.Background(), configKey("acme"))
	if strings.Contains(string(after), "authored_yaml") || !strings.Contains(string(after), `"collection"`) {
		t.Fatalf("write did not migrate storage: %s", after)
	}
}

func TestInvalidLegacyCollectionCanBeRepairedThroughAdminAPI(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	cfg, version, _ := manager.LoadConfig(context.Background())
	cfg.Collection, cfg.AuthoredYAML = nil, "mode: daemon"
	if err := replaceRecord(context.Background(), store, configKey("acme"), version, cfg); err != nil {
		t.Fatal(err)
	}
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("cannot inspect broken config: %d %s", w.Code, w.Body)
	}
	var response FleetConfig
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Collection != nil || response.CollectionError == "" || response.AuthoredYAML != cfg.AuthoredYAML {
		t.Fatalf("broken config was silently reset: %s", w.Body)
	}
	if cfg, _, err := manager.LoadConfig(context.Background()); err != nil || cfg.CollectionError == "" {
		t.Fatalf("invalid collection must remain readable for admin and telemetry: %+v %v", cfg, err)
	}
	body, _ := json.Marshal(adminConfigRequest{AgeRecipients: cfg.AgeRecipients, Collection: &CollectionConfig{DrainDeadline: ptr("30s")}})
	req := adminRequest("PUT", "/v1/admin/orgs/acme/config", string(body), testAdminCredential)
	req.Header.Set("If-Match", w.Header().Get("ETag"))
	w = serveAdmin(t, server, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("repair rejected: %d %s", w.Code, w.Body)
	}
	repaired, _, err := manager.LoadConfig(context.Background())
	if err != nil || *repaired.Collection.DrainDeadline != "30s" {
		t.Fatalf("repair not served: %+v %v", repaired, err)
	}
}

func TestLegacyCollectionRefusesAmbiguousOrUnknownSettings(t *testing.T) {
	for _, raw := range []string{"mode: daemon", "mode: {}\n---\nmax_files_per_run: 1", "sources: [{id: claude-code, typo: false}]", "encryption: {}"} {
		cfg := FleetConfig{AuthoredYAML: raw}
		if err := normalizeCollection(&cfg); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	cfg := FleetConfig{AuthoredYAML: "drain_deadline: 30s", Collection: &CollectionConfig{}}
	if err := normalizeCollection(&cfg); err == nil {
		t.Fatal("accepted both collection and YAML")
	}
}

func TestLegacyCollectionKeepsTolerantReadSemantics(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	cfg, version, _ := manager.LoadConfig(context.Background())
	cfg.Collection, cfg.AuthoredYAML = nil, "mode: {schedule: 5s}\nmax_files_per_run: null"
	if err := replaceRecord(context.Background(), store, configKey("acme"), version, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, version, err := manager.LoadConfig(context.Background())
	if err != nil || loaded.CollectionError != "" || *loaded.Collection.Mode.Schedule != "5s" {
		t.Fatalf("legacy read changed: %+v %v", loaded, err)
	}
	if err := manager.ApplyConfig(context.Background(), loaded, version); err == nil {
		t.Fatal("new writes must use the canonical schema")
	}
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("legacy collection blocked organization listing: %d %s", w.Code, w.Body)
	}
}

func TestStoredCollectionOutsideTheWriteSchemaStaysServable(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	raw, version, _ := store.Get(context.Background(), configKey("acme"))
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	// What a later, stricter schema makes of a value an earlier one accepted.
	record["collection"] = map[string]any{"mode": map[string]any{"schedule": "30s"}}
	raw, _ = json.Marshal(record)
	if err := store.Replace(context.Background(), configKey("acme"), version, raw); err != nil {
		t.Fatal(err)
	}
	loaded, version, err := manager.LoadConfig(context.Background())
	if err != nil || loaded.CollectionError != "" || !strings.Contains(renderConfig(loaded), "schedule: 30s") {
		t.Fatalf("stored collection no longer served: %+v %v", loaded, err)
	}
	if w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential)); w.Code != http.StatusOK {
		t.Fatalf("admin cannot load the config to repair it: %d %s", w.Code, w.Body)
	}
	err = manager.ApplyConfig(context.Background(), loaded, version)
	if err == nil || !strings.Contains(err.Error(), "mode.schedule") || strings.Contains(err.Error(), "file://") {
		t.Fatalf("write must name the field to correct: %v", err)
	}
}

func TestLegacyCollectionServesWithoutKeysTheShipperIgnores(t *testing.T) {
	server, store := testAdminServer(t)
	manager, _ := server.manager.ForOrganization("acme")
	cfg, version, _ := manager.LoadConfig(context.Background())
	cfg.Collection, cfg.AuthoredYAML = nil, "sources: [{id: claude-code, future_key: 1}]"
	if err := replaceRecord(context.Background(), store, configKey("acme"), version, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, version, err := manager.LoadConfig(context.Background())
	if err != nil || loaded.CollectionError != "" || strings.Contains(renderConfig(loaded), "future_key") {
		t.Fatalf("unknown legacy key blocked serving: %+v %v", loaded, err)
	}
	if err := manager.ApplyConfig(context.Background(), loaded, version); err != nil {
		t.Fatalf("a write of the served settings must succeed: %v", err)
	}
}
