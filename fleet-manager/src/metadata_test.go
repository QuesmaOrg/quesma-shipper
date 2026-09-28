package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
)

func patchMetadata(t *testing.T, server *Server, id, body string) int {
	t.Helper()
	return serveAdmin(t, server, adminRequest("PATCH", "/v1/admin/orgs/acme/installs/"+id+"/metadata", body, testAdminCredential)).Code
}

func TestMetadataValidationAndNamePreservation(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	for _, body := range []string{
		`{}`, `{"metadata":null}`, `{"metadata":{"Bad":"value"}}`,
		`{"metadata":{"email":"two\nlines"}}`, `{"metadata":{"email":42}}`,
		fmt.Sprintf(`{"metadata":{"email":%q}}`, strings.Repeat("a", 257)),
	} {
		if status := patchMetadata(t, server, id, body); status != http.StatusBadRequest {
			t.Fatalf("invalid metadata %s: status %d", body, status)
		}
	}
	if code := patchMetadata(t, server, id, `{"metadata":{"email":"alice@example.com","team":"engineering"}}`); code != http.StatusNoContent {
		t.Fatalf("metadata update = %d", code)
	}
	if records := listTags(t, server); len(records) != 1 || records[0].Name != "" || records[0].Metadata["email"] != "alice@example.com" {
		t.Fatalf("metadata without a name: %+v", records)
	}
	if code := setTag(t, server, id, `{"name":"Custom laptop"}`); code != http.StatusNoContent {
		t.Fatalf("rename = %d", code)
	}
	if code := setTag(t, server, id, `{"name":"MDM name","if_missing":true}`); code != http.StatusNoContent {
		t.Fatalf("fill missing name = %d", code)
	}
	if code := patchMetadata(t, server, id, `{"metadata":{"team":null,"office":"Warsaw"}}`); code != http.StatusNoContent {
		t.Fatalf("metadata merge = %d", code)
	}
	record := listTags(t, server)[0]
	if record.Name != "Custom laptop" || record.Metadata["email"] != "alice@example.com" || record.Metadata["office"] != "Warsaw" || len(record.Metadata) != 2 {
		t.Fatalf("merge erased fields: %+v", record)
	}
	max := make(map[string]*string)
	for i := range 16 {
		max[fmt.Sprintf("key_%d", i)] = ptr(strings.Repeat("界", 256))
	}
	max["email"], max["office"] = nil, nil
	manager, _ := server.manager.ForOrganization("acme")
	if err := manager.PatchMetadata(context.Background(), id, max); err != nil {
		t.Fatalf("16 keys with Unicode values: %v", err)
	}
	if code := patchMetadata(t, server, id, `{"metadata":{"extra":"too many"}}`); code != http.StatusBadRequest {
		t.Fatalf("17 keys = %d", code)
	}
}

// tagsInterleavingStore runs another writer between a tags read and its write, once.
type tagsInterleavingStore struct {
	ObjectStore
	beforeWrite func()
}

func (s *tagsInterleavingStore) interleave(key string) {
	if strings.HasSuffix(key, "/tags.json") && s.beforeWrite != nil {
		hook := s.beforeWrite
		s.beforeWrite = nil
		hook()
	}
}

func (s *tagsInterleavingStore) Create(ctx context.Context, key string, raw []byte) error {
	s.interleave(key)
	return s.ObjectStore.Create(ctx, key, raw)
}

func (s *tagsInterleavingStore) Replace(ctx context.Context, key, version string, raw []byte) error {
	s.interleave(key)
	return s.ObjectStore.Replace(ctx, key, version, raw)
}

func TestTagsRetryPreservesConcurrentMetadataAndCustomName(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	manager, _ := server.manager.ForOrganization("acme")
	ctx := context.Background()
	// No tags record yet: the first attempt is a Create that loses to the interleaved writer, and
	// the retry is a Replace against what that writer left.
	interleaved := &tagsInterleavingStore{ObjectStore: store}
	manager.store = interleaved
	interleaved.beforeWrite = func() {
		other, _ := server.manager.ForOrganization("acme")
		if err := other.PatchMetadata(ctx, id, map[string]*string{"email": ptr("alice@example.com")}); err != nil {
			t.Fatal(err)
		}
		if err := other.SetTag(ctx, id, "Custom laptop"); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.setTag(ctx, id, "Imported laptop", true); err != nil {
		t.Fatal(err)
	}
	record, _, err := manager.LoadTags(ctx, id)
	if err != nil || record.Name != "Custom laptop" || record.Metadata["email"] != "alice@example.com" {
		t.Fatalf("concurrent values lost: %+v %v", record, err)
	}
}

func TestMetadataImportMatchesWithinOrganizationAndResolvesManually(t *testing.T) {
	server, store := testAdminServer(t)
	ids := []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333"}
	// What os.Hostname() reports: the Bonjour name on a Mac, a DNS name on a managed network.
	for i, id := range ids {
		plantInstall(t, server, store, id)
		record, version, _ := getRecord[InstallRecord](context.Background(), store, installKey("acme", id))
		record.Hostname = []string{"Unique-MacBook.local", "shared", "SHARED.corp.example.com"}[i]
		if err := replaceRecord(context.Background(), store, installKey("acme", id), version, record); err != nil {
			t.Fatal(err)
		}
	}
	importRows := func(body string) []metadataImportResult {
		t.Helper()
		w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/installs/metadata/import", body, testAdminCredential))
		if w.Code != http.StatusOK {
			t.Fatalf("import = %d: %s", w.Code, w.Body.String())
		}
		var result struct {
			Results []metadataImportResult `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Results
	}
	// The inventory carries the computer name as the device manager kept it.
	results := importRows(`{"rows":[{"hostname":"unique-macbook","metadata":{"email":"alice@example.com"}},{"hostname":"Shared","metadata":{"email":"bob@example.com"}},{"hostname":"nobody","metadata":{"email":"c@example.com"}}]}`)
	if results[0].Status != "imported" || results[0].InstallID != ids[0] || results[1].Status != "ambiguous" || len(results[1].Candidates) != 2 || results[2].Status != "unmatched" {
		t.Fatalf("matching results: %+v", results)
	}
	if record, _, err := acmeManager(t, server).LoadTags(context.Background(), ids[0]); err != nil || record.Metadata["email"] != "alice@example.com" {
		t.Fatalf("imported metadata: %+v %v", record, err)
	}
	// Two rows that fold to one hostname are two rows for one machine, whatever their spelling.
	results = importRows(`{"rows":[{"hostname":"unique-macbook","metadata":{"email":"one@example.com"}},{"hostname":"UNIQUE-MACBOOK.local","metadata":{"email":"two@example.com"}}]}`)
	if results[0].Status != "ambiguous" || results[1].Status != "ambiguous" {
		t.Fatalf("duplicate inventory rows: %+v", results)
	}
	results = importRows(fmt.Sprintf(`{"rows":[{"hostname":"unmatched","install_id":%q,"metadata":{"email":"manual@example.com"}},{"hostname":"foreign","install_id":"44444444-4444-4444-4444-444444444444","metadata":{}}]}`, ids[1]))
	if results[0].Status != "imported" || results[1].Status != "unmatched" {
		t.Fatalf("manual selection results: %+v", results)
	}
	results = importRows(fmt.Sprintf(`{"rows":[{"hostname":"a","install_id":%q,"metadata":{"email":"a@example.com"}},{"hostname":"b","install_id":%q,"metadata":{"email":"b@example.com"}}]}`, ids[0], ids[0]))
	if results[0].Status != "ambiguous" || results[1].Status != "ambiguous" {
		t.Fatalf("repeated targets: %+v", results)
	}
	for _, body := range []string{
		`{"rows":[{"hostname":"unique-macbook","metadata":{"email":null}}]}`,
		`{"rows":[{"hostname":" . ","metadata":{"email":"x@example.com"}}]}`,
	} {
		if w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/installs/metadata/import", body, testAdminCredential)); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid import %s = %d", body, w.Code)
		}
	}
}

func acmeManager(t *testing.T, server *Server) *Manager {
	t.Helper()
	manager, err := server.manager.ForOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestHostnameMatchKey(t *testing.T) {
	for input, want := range map[string]string{
		"Alices-MacBook.local": "alices-macbook", "build-01.corp.example.com": "build-01",
		"  Plain  ": "plain", "": "", ".local": "",
	} {
		if got := hostnameMatchKey(input); got != want {
			t.Errorf("hostnameMatchKey(%q) = %q, want %q", input, got, want)
		}
	}
}

// The tags object is the one the service writes outside control/; nothing to say means no object.
func TestEmptyTagsUpdateWritesNothing(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	for _, step := range []func() int{
		func() int { return patchMetadata(t, server, id, `{"metadata":{}}`) },
		func() int { return patchMetadata(t, server, id, `{"metadata":{"absent":null}}`) },
		func() int { return setTag(t, server, id, `{"name":""}`) },
	} {
		if code := step(); code != http.StatusNoContent {
			t.Fatalf("empty update = %d", code)
		}
	}
	if _, _, err := store.Get(context.Background(), tagsKey("acme", id)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty update created tags.json: %v", err)
	}
	if records := listTags(t, server); len(records) != 0 {
		t.Fatalf("listed a record that was never written: %+v", records)
	}
}

func TestListTagsKeepsNameWhenMetadataIsUnusable(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	metadata := make(map[string]string)
	for i := range 17 {
		metadata[fmt.Sprintf("key_%d", i)] = "later version"
	}
	rec := TagsRecord{Schema: schemaVersion, InstallID: id, Name: "Kept", Metadata: metadata, UpdatedAt: server.manager.time()}
	if err := createRecord(context.Background(), store, tagsKey("acme", id), rec); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	server.logger = log.New(&logged, "", 0)
	records := listTags(t, server)
	if len(records) != 1 || records[0].Name != "Kept" || records[0].Metadata != nil {
		t.Fatalf("unusable metadata: %+v", records)
	}
	if !strings.Contains(logged.String(), "unusable metadata") {
		t.Fatalf("nothing logged: %q", logged.String())
	}
}

type failingTagsStore struct {
	ObjectStore
}

func (s failingTagsStore) Create(ctx context.Context, key string, raw []byte) error {
	if strings.HasSuffix(key, "/tags.json") {
		return errors.New("bucket on fire")
	}
	return s.ObjectStore.Create(ctx, key, raw)
}

func TestMetadataImportLogsStorageFailures(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	manager := acmeManager(t, server)
	manager.store = failingTagsStore{store}
	var logged bytes.Buffer
	results, err := manager.ImportMetadata(context.Background(), []metadataImportRow{{Hostname: "any", InstallID: id, Metadata: map[string]*string{"email": ptr("a@example.com")}}}, log.New(&logged, "", 0))
	if err != nil || results[0].Status != "error" || strings.Contains(results[0].Message, "fire") {
		t.Fatalf("storage failure result: %+v %v", results, err)
	}
	if !strings.Contains(logged.String(), "bucket on fire") || !strings.Contains(logged.String(), id) {
		t.Fatalf("failure not logged: %q", logged.String())
	}
}
