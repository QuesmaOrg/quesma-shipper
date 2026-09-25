package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func plantInstall(t *testing.T, server *Server, store *memoryStore, installID string) {
	t.Helper()
	now := server.manager.time()
	record := InstallRecord{Schema: schemaVersion, Organization: "acme", InstallID: installID,
		Status: InstallActive, CreatedAt: now, UpdatedAt: now}
	raw, err := encodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), installKey("acme", installID), raw); err != nil {
		t.Fatal(err)
	}
}

func setTag(t *testing.T, server *Server, installID, body string) int {
	t.Helper()
	target := "/v1/admin/orgs/acme/installs/" + installID + "/tags"
	return serveAdmin(t, server, adminRequest("PUT", target, body, testAdminCredential)).Code
}

func listTags(t *testing.T, server *Server) []TagsRecord {
	t.Helper()
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/installs/tags", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("list tags = %d: %s", w.Code, w.Body.String())
	}
	var out []TagsRecord
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("list tags body %q: %v", w.Body.String(), err)
	}
	return out
}

// The name lands in the install's own root, not under control/, because that is what lets whatever
// reads the objects read the name beside them.
func TestSetTagWritesTheInstallRoot(t *testing.T) {
	server, store := testAdminServer(t)
	installID := "11111111-1111-1111-1111-111111111111"
	plantInstall(t, server, store, installID)

	if code := setTag(t, server, installID, `{"name":"Rafal's laptop"}`); code != http.StatusNoContent {
		t.Fatalf("set name = %d", code)
	}
	key := "v1/organization=acme/install=" + installID + "/tags.json"
	rec, _, err := getRecord[TagsRecord](context.Background(), store, key)
	if err != nil {
		t.Fatalf("no record at %s: %v", key, err)
	}
	if rec.Schema != schemaVersion || rec.InstallID != installID || rec.Name != "Rafal's laptop" {
		t.Fatalf("record: %+v", rec)
	}
	if rec.UpdatedAt.IsZero() {
		t.Fatal("record carries no updated_at")
	}
	if records := listTags(t, server); len(records) != 1 || records[0].Name != "Rafal's laptop" {
		t.Fatalf("list: %+v", records)
	}
}

func TestSetTagRenamesAndClears(t *testing.T) {
	server, store := testAdminServer(t)
	installID := "22222222-2222-2222-2222-222222222222"
	plantInstall(t, server, store, installID)

	if code := setTag(t, server, installID, `{"name":"first"}`); code != http.StatusNoContent {
		t.Fatalf("set = %d", code)
	}
	if code := setTag(t, server, installID, `{"name":"second"}`); code != http.StatusNoContent {
		t.Fatalf("rename = %d", code)
	}
	if records := listTags(t, server); len(records) != 1 || records[0].Name != "second" {
		t.Fatalf("rename did not take: %+v", records)
	}
	// An empty name is how an install goes back to showing its id, so it is a clear, not a refusal.
	if code := setTag(t, server, installID, `{"name":""}`); code != http.StatusNoContent {
		t.Fatalf("clear = %d", code)
	}
	if records := listTags(t, server); len(records) != 0 {
		t.Fatalf("cleared name still listed: %+v", records)
	}
}

func TestSetTagRefusesUnusableNames(t *testing.T) {
	server, store := testAdminServer(t)
	installID := "33333333-3333-3333-3333-333333333333"
	plantInstall(t, server, store, installID)

	for _, tc := range []struct{ what, body string }{
		{"surrounding whitespace", `{"name":"  padded  "}`},
		{"too long", fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", 101))},
		{"control character", `{"name":"two\nlines"}`},
	} {
		if code := setTag(t, server, installID, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", tc.what, code)
		}
	}
	if records := listTags(t, server); len(records) != 0 {
		t.Fatalf("a refused name was stored: %+v", records)
	}
}

// A name may not conjure a root: without the install record any UUID would place a file under a
// prefix of its choosing.
func TestSetTagRefusesUnknownInstallAndWritesNothing(t *testing.T) {
	server, store := testAdminServer(t)
	unknown := "44444444-4444-4444-4444-444444444444"

	if code := setTag(t, server, unknown, `{"name":"nobody"}`); code != http.StatusNotFound {
		t.Fatalf("unknown install = %d, want 404", code)
	}
	if code := setTag(t, server, "not-a-uuid", `{"name":"nobody"}`); code != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400", code)
	}
	objects, err := store.List(context.Background(), "v1/organization=acme/install=")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("wrote into the install prefix: %v", objects)
	}
}

func TestListTagsSkipsNamesItCannotUse(t *testing.T) {
	server, store := testAdminServer(t)
	good := "55555555-5555-5555-5555-555555555555"
	broken := "66666666-6666-6666-6666-666666666666"
	mismatched := "77777777-7777-7777-7777-777777777777"
	for _, id := range []string{good, broken, mismatched} {
		plantInstall(t, server, store, id)
	}
	if code := setTag(t, server, good, `{"name":"readable"}`); code != http.StatusNoContent {
		t.Fatalf("set = %d", code)
	}
	ctx := context.Background()
	if err := store.Create(ctx, tagsKey("acme", broken), []byte("{ not json")); err != nil {
		t.Fatal(err)
	}
	wrong, _ := encodeRecord(TagsRecord{Schema: schemaVersion, InstallID: good, Name: "wrong root"})
	if err := store.Create(ctx, tagsKey("acme", mismatched), wrong); err != nil {
		t.Fatal(err)
	}

	records := listTags(t, server)
	if len(records) != 1 || records[0].InstallID != good {
		t.Fatalf("list: %+v", records)
	}
}

func TestListTagsReadsTheWholeFleetConcurrently(t *testing.T) {
	server, store := testAdminServer(t)
	for i := range 40 {
		id := fmt.Sprintf("88888888-8888-8888-8888-%012d", i)
		plantInstall(t, server, store, id)
		if code := setTag(t, server, id, fmt.Sprintf(`{"name":"machine %d"}`, i)); code != http.StatusNoContent {
			t.Fatalf("set %s = %d", id, code)
		}
	}
	store.getDelay, store.delayGetIn = 20*time.Millisecond, "/tags.json"

	start := time.Now()
	records := listTags(t, server)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("40 names took %s, so the reads are not concurrent", elapsed)
	}
	if len(records) != 40 {
		t.Fatalf("listed %d names, want 40", len(records))
	}
}

// Names are a separate document and a separate endpoint, so the installs list must not grow one.
func TestTagsEndpointIsSeparateFromInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	installID := "99999999-9999-9999-9999-999999999999"
	plantInstall(t, server, store, installID)
	if code := setTag(t, server, installID, `{"name":"separate"}`); code != http.StatusNoContent {
		t.Fatalf("set = %d", code)
	}
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/installs", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("list installs = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "separate") {
		t.Fatalf("the name leaked into the installs list: %s", w.Body.String())
	}
}
