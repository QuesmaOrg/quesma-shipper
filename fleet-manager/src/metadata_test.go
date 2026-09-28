package main

import (
	"context"
	"encoding/json"
	"fmt"
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

type tagsInterleavingStore struct {
	ObjectStore
	beforeReplace func()
}

func (s *tagsInterleavingStore) Replace(ctx context.Context, key, version string, raw []byte) error {
	if strings.HasSuffix(key, "/tags.json") && s.beforeReplace != nil {
		hook := s.beforeReplace
		s.beforeReplace = nil
		hook()
	}
	return s.ObjectStore.Replace(ctx, key, version, raw)
}

func TestTagsRetryPreservesConcurrentMetadataAndCustomName(t *testing.T) {
	server, store := testAdminServer(t)
	id := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, id)
	manager, _ := server.manager.ForOrganization("acme")
	ctx := context.Background()
	if err := manager.SetTag(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	interleaved := &tagsInterleavingStore{ObjectStore: store}
	manager.store = interleaved
	interleaved.beforeReplace = func() {
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
	for i, id := range ids {
		plantInstall(t, server, store, id)
		record, version, _ := getRecord[InstallRecord](context.Background(), store, installKey("acme", id))
		record.Hostname = []string{"unique", "shared", "shared"}[i]
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
	results := importRows(`{"rows":[{"hostname":"unique","metadata":{"email":"alice@example.com"}},{"hostname":"shared","metadata":{"email":"bob@example.com"}},{"hostname":"Unique","metadata":{"email":"c@example.com"}}]}`)
	if results[0].Status != "imported" || results[1].Status != "ambiguous" || len(results[1].Candidates) != 2 || results[2].Status != "unmatched" {
		t.Fatalf("matching results: %+v", results)
	}
	results = importRows(`{"rows":[{"hostname":"unique","metadata":{"email":"one@example.com"}},{"hostname":"unique","metadata":{"email":"two@example.com"}}]}`)
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
	w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs/acme/installs/metadata/import", `{"rows":[{"hostname":"unique","metadata":{"email":null}}]}`, testAdminCredential))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("null import metadata = %d", w.Code)
	}
}
