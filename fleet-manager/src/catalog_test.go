package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func testCatalog(features ...string) *sourceCatalog {
	return &sourceCatalog{
		Sources: []catalogSource{
			{ID: "claude-code-transcripts", Family: "claude-code", FamilyName: "Claude Code", Enabled: true,
				Roots: []string{"$CLAUDE_CONFIG_DIR", "~/.claude"}, Include: []string{"projects/**/*.jsonl"}, Exclude: []string{"projects/**/*.png"}},
			{ID: "codex-rollouts", Family: "codex", FamilyName: "Codex", Enabled: true, Roots: []string{"~/.codex"}},
		},
		RulePacks: []string{"gitleaks-core", "pii-core"},
		Features:  features,
	}
}

// enrollInstall adds one more active install to the organization enrolledServer made.
func enrollInstall(t *testing.T, server *Server, manager *Manager) (ed25519.PrivateKey, string) {
	t.Helper()
	identity, _ := age.GenerateX25519Identity()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	installID := uuid.NewString()
	token, _ := manager.CreateGrant(context.Background(), manager.time().Add(time.Hour))
	body, _ := json.Marshal(enrollRequest{Grant: token, InstallID: installID, DevicePublicKey: base64.StdEncoding.EncodeToString(public), AgeRecipient: identity.Recipient().String()})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", recorder.Code, recorder.Body.String())
	}
	return private, installID
}

func setCollection(t *testing.T, manager *Manager, collection *CollectionConfig) {
	t.Helper()
	cfg, version, err := manager.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Collection = collection
	if err := manager.ApplyConfig(context.Background(), cfg, version); err != nil {
		t.Fatal(err)
	}
}

// fetchCollection asks for the install's document, as a build reporting catalog would, and
// returns the collection it was served.
func fetchCollection(t *testing.T, server *Server, key ed25519.PrivateKey, installID string, catalog *sourceCatalog) CollectionConfig {
	t.Helper()
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}, Catalog: catalog})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, signedRequest(http.MethodPost, "/v1/config", body, installID, key, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config: %d %s", recorder.Code, recorder.Body.String())
	}
	var response configResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var served CollectionConfig
	if err := yaml.Unmarshal(response.Config, &served); err != nil {
		t.Fatal(err)
	}
	return served
}

func servedSource(t *testing.T, served CollectionConfig, id string) CollectionSource {
	t.Helper()
	for _, source := range served.Sources {
		if source.ID == id {
			return source
		}
	}
	t.Fatalf("source %s was not served: %+v", id, served.Sources)
	return CollectionSource{}
}

func TestConfigFetchStoresTheCatalogOncePerBuild(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	otherKey, otherID := enrollInstall(t, server, manager)
	fetchCollection(t, server, key, installID, testCatalog())
	fetchCollection(t, server, otherKey, otherID, testCatalog())

	rec, other := loadSeen(t, manager, installID), loadSeen(t, manager, otherID)
	if rec.CatalogDigest == "" || rec.CatalogDigest != other.CatalogDigest || rec.LastConfigAt == nil {
		t.Fatalf("one build, two digests or none: %q %q", rec.CatalogDigest, other.CatalogDigest)
	}
	stored, err := manager.loadCatalog(context.Background(), rec.CatalogDigest)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := json.Marshal(testCatalog()); !bytes.Equal(mustJSON(t, stored), want) {
		t.Fatalf("stored catalog %s", mustJSON(t, stored))
	}
	objects, _ := manager.store.List(context.Background(), controlPrefix("acme")+"catalogs/")
	if len(objects) != 1 {
		t.Fatalf("one build stored %d catalogs", len(objects))
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The throttle collapses repeated check-ins, but a different catalog is a different answer to
// what this install can execute, so it is recorded at once -- and so is reporting none.
func TestChangedCatalogIsRecordedInsideTheThrottle(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	store := manager.store.(*memoryStore)
	fetchCollection(t, server, key, installID, testCatalog())
	first := loadSeen(t, manager, installID).CatalogDigest
	versions := store.versionOf(seenKey("acme", installID))

	fetchCollection(t, server, key, installID, testCatalog())
	if got := store.versionOf(seenKey("acme", installID)); got != versions {
		t.Fatalf("an unchanged catalog inside the throttle wrote again: %d -> %d", versions, got)
	}

	fetchCollection(t, server, key, installID, testCatalog(featureExcludeAdd))
	second := loadSeen(t, manager, installID).CatalogDigest
	if second == "" || second == first {
		t.Fatalf("a changed catalog inside the throttle was not recorded: %q -> %q", first, second)
	}

	fetchCollection(t, server, key, installID, nil)
	if got := loadSeen(t, manager, installID).CatalogDigest; got != "" {
		t.Fatalf("a fetch without a catalog left %q on record", got)
	}

	// Other events say nothing about the catalog and must not clear it.
	fetchCollection(t, server, key, installID, testCatalog())
	authorize := authorizeBody(t, manager, installID)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, signedRequest(http.MethodPost, "/v2/uploads/authorize", authorize, installID, key, uploadAuthorizePreamble))
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", recorder.Code, recorder.Body.String())
	}
	if rec := loadSeen(t, manager, installID); rec.LastVendAt == nil || rec.CatalogDigest != first {
		t.Fatalf("a vend changed the catalog on record: %+v", rec)
	}
}

// A catalog this service cannot act on is treated as absent: the fetch still succeeds.
func TestUnusableCatalogIsIgnored(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	var logs bytes.Buffer
	server.logger = log.New(&logs, "", 0)
	catalog := testCatalog()
	catalog.Sources[1].ID = ""
	fetchCollection(t, server, key, installID, catalog)
	if got := loadSeen(t, manager, installID).CatalogDigest; got != "" {
		t.Fatalf("an unusable catalog was recorded as %q", got)
	}
	if !strings.Contains(logs.String(), "ignored its catalog") {
		t.Fatalf("not logged: %q", logs.String())
	}
}

func TestServedDocumentLeavesOutWhatTheBuildLacks(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	var logs bytes.Buffer
	server.logger = log.New(&logs, "", 0)
	setCollection(t, manager, &CollectionConfig{
		Sources: []CollectionSource{{ID: "cursor-chats", Enabled: ptr(true)}, {ID: "claude-code-transcripts", Enabled: ptr(false)}},
		Scrub:   &CollectionScrub{RulePacks: []string{"made-up", "gitleaks-core"}, SecretKeyNames: []string{"api_key"}},
	})
	for range 3 {
		served := fetchCollection(t, server, key, installID, testCatalog())
		if len(served.Sources) != 1 || served.Sources[0].ID != "claude-code-transcripts" {
			t.Fatalf("served sources %+v", served.Sources)
		}
		if served.Scrub == nil || !slices.Equal(served.Scrub.RulePacks, []string{"gitleaks-core"}) || !slices.Equal(served.Scrub.SecretKeyNames, []string{"api_key"}) {
			t.Fatalf("served scrub %+v", served.Scrub)
		}
	}
	if lines := strings.Count(logs.String(), "\n"); lines != 1 || !strings.Contains(logs.String(), `sources[0] "cursor-chats"`) || !strings.Contains(logs.String(), `scrub.rule_packs[0] "made-up"`) {
		t.Fatalf("want one line naming both, got %q", logs.String())
	}

	// A build without a catalog is served as before.
	served := fetchCollection(t, server, key, installID, nil)
	if len(served.Sources) != 2 || len(served.Scrub.RulePacks) != 2 {
		t.Fatalf("a catalog-less build was filtered: %+v", served)
	}
}

func TestExcludeAddIsServedOnlyToABuildThatReadsIt(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	setCollection(t, manager, &CollectionConfig{Sources: []CollectionSource{
		{ID: "claude-code-transcripts", ExcludeAdd: []string{"projects/-Users-ada-client/**"}},
		{ID: "codex-rollouts", Exclude: []string{"sessions/old/**"}, ExcludeAdd: []string{"sessions/client/**"}},
	}})

	served := fetchCollection(t, server, key, installID, testCatalog(featureExcludeAdd))
	claude := servedSource(t, served, "claude-code-transcripts")
	if claude.Exclude != nil || !slices.Equal(claude.ExcludeAdd, []string{"projects/-Users-ada-client/**"}) {
		t.Fatalf("a build that reads exclude_add got %+v", claude)
	}

	served = fetchCollection(t, server, key, installID, testCatalog())
	claude, codex := servedSource(t, served, "claude-code-transcripts"), servedSource(t, served, "codex-rollouts")
	// Over the build's own excludes when the entry sets none, over the entry's when it does.
	if claude.ExcludeAdd != nil || !slices.Equal(claude.Exclude, []string{"projects/**/*.png", "projects/-Users-ada-client/**"}) {
		t.Fatalf("folded over the catalog: %+v", claude)
	}
	if codex.ExcludeAdd != nil || !slices.Equal(codex.Exclude, []string{"sessions/old/**", "sessions/client/**"}) {
		t.Fatalf("folded over the entry: %+v", codex)
	}
	if stored, _, _ := manager.LoadConfig(context.Background()); stored.Collection.Sources[0].Exclude != nil {
		t.Fatalf("folding changed the stored collection: %+v", stored.Collection.Sources[0])
	}
}

func TestCatalogLessFetchFoldsOverTheOrganizationsNewestCatalog(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	var logs bytes.Buffer
	server.logger = log.New(&logs, "", 0)
	olderKey, olderID := enrollInstall(t, server, manager)
	newerKey, newerID := enrollInstall(t, server, manager)

	older := testCatalog()
	older.Sources[0].Exclude = []string{"older/**"}
	fetchCollection(t, server, olderKey, olderID, older)
	manager.now = func() time.Time { return time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC) }
	newer := testCatalog(featureExcludeAdd)
	newer.Sources[0].Exclude = []string{"newer/**"}
	fetchCollection(t, server, newerKey, newerID, newer)

	setCollection(t, manager, &CollectionConfig{Sources: []CollectionSource{
		{ID: "claude-code-transcripts", ExcludeAdd: []string{"client/**"}},
		{ID: "cursor-chats", ExcludeAdd: []string{"client/**"}},
	}})
	for range 2 {
		served := fetchCollection(t, server, key, installID, nil)
		claude, cursor := servedSource(t, served, "claude-code-transcripts"), servedSource(t, served, "cursor-chats")
		if claude.ExcludeAdd != nil || !slices.Equal(claude.Exclude, []string{"newer/**", "client/**"}) {
			t.Fatalf("not folded over the newest catalog: %+v", claude)
		}
		// No catalog has it, so there is nothing to fold over: dropped, never served unfolded.
		if cursor.ExcludeAdd != nil || cursor.Exclude != nil {
			t.Fatalf("an unfoldable exclude_add was served: %+v", cursor)
		}
	}
	if strings.Count(logs.String(), "dropped sources[1]") != 1 {
		t.Fatalf("the drop was not logged once: %q", logs.String())
	}
}

// seedInstall writes an install as the store would hold it, and the catalog its latest fetch
// carried, if any.
func seedInstall(t *testing.T, store *memoryStore, status InstallStatus, catalog *sourceCatalog, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := createRecord(ctx, store, installKey("acme", id), InstallRecord{Schema: schemaVersion, Organization: "acme", InstallID: id, Status: status}); err != nil {
		t.Fatal(err)
	}
	if status != InstallPending {
		reportCatalog(t, store, id, catalog, at)
	}
	return id
}

// reportCatalog records a config fetch at the given time that carried catalog, or none.
func reportCatalog(t *testing.T, store *memoryStore, id string, catalog *sourceCatalog, at time.Time) {
	t.Helper()
	ctx := context.Background()
	seen := SeenRecord{Schema: schemaVersion, InstallID: id, LastConfigAt: &at, LastSeenAt: at}
	if catalog != nil {
		reported, err := catalog.reported()
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Create(ctx, catalogKey("acme", reported.Digest), reported.Record)
		seen.CatalogDigest = reported.Digest
	}
	raw, _ := encodeRecord(seen)
	if err := store.Put(ctx, seenKey("acme", id), raw); err != nil {
		t.Fatal(err)
	}
}

func listSources(t *testing.T, server *Server) (adminSourcesResponse, string) {
	t.Helper()
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/sources", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("sources: %d %s", w.Code, w.Body.String())
	}
	var out adminSourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out, w.Body.String()
}

func TestSourcesEndpointWithNoReportingInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	seedInstall(t, store, InstallActive, nil, time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))
	out, raw := listSources(t, server)
	if out.Installs != (adminSourcesInstalls{Active: 1, Reporting: 0}) {
		t.Fatalf("installs %+v", out.Installs)
	}
	for _, want := range []string{`"sources":[]`, `"rule_packs":[]`, `"features":[]`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("%s missing from %s", want, raw)
		}
	}
}

func TestSourcesEndpointCountsReportingActiveInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	at := func(hour int) time.Time { return time.Date(2026, 9, 1, hour, 0, 0, 0, time.UTC) }
	older := testCatalog()
	older.Sources[0].Description = "older words"
	newer := testCatalog(featureExcludeAdd)
	newer.Sources[0].Description = "newer words"
	newer.Sources = append(newer.Sources, catalogSource{ID: "aider-history", Family: "aider", FamilyName: "Aider", Enabled: false, Roots: []string{"~"}})
	newer.RulePacks = append(newer.RulePacks, "cloud-keys")
	seedInstall(t, store, InstallActive, older, at(9))
	seedInstall(t, store, InstallActive, newer, at(10))
	seedInstall(t, store, InstallActive, nil, at(11))
	// Neither of these counts, whatever they reported.
	revoked := testCatalog(featureExcludeAdd)
	revoked.Sources = append(revoked.Sources, catalogSource{ID: "revoked-only", Family: "x", Enabled: true, Roots: []string{"~"}})
	seedInstall(t, store, InstallRevoked, revoked, at(11))
	seedInstall(t, store, InstallPending, nil, at(11))

	out, raw := listSources(t, server)
	if out.Installs != (adminSourcesInstalls{Active: 3, Reporting: 2}) {
		t.Fatalf("installs %+v", out.Installs)
	}
	var ids []string
	for _, source := range out.Sources {
		ids = append(ids, source.ID)
	}
	if !slices.Equal(ids, []string{"aider-history", "claude-code-transcripts", "codex-rollouts"}) {
		t.Fatalf("sources not sorted by family name then id, or a non-counting install leaked: %v", ids)
	}
	claude := out.Sources[1]
	if claude.Installs != 2 || claude.Description != "newer words" || claude.FamilyName != "Claude Code" || out.Sources[0].Installs != 1 {
		t.Fatalf("counts or metadata: %+v", out.Sources)
	}
	if !strings.Contains(raw, `"enrichers":[]`) || strings.Contains(raw, "null") {
		t.Fatalf("an empty list is not an empty array: %s", raw)
	}
	wantPacks := []adminNameCount{{"cloud-keys", 1}, {"gitleaks-core", 2}, {"pii-core", 2}}
	if !slices.Equal(out.RulePacks, wantPacks) || !slices.Equal(out.Features, []adminNameCount{{featureExcludeAdd, 1}}) {
		t.Fatalf("packs %+v features %+v", out.RulePacks, out.Features)
	}

	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/nobody/sources", "", testAdminCredential))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown organization: %d", w.Code)
	}
}

func putCollection(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	req := adminRequest("PUT", "/v1/admin/orgs/acme/config", body, testAdminCredential)
	req.Header.Set("If-Match", w.Header().Get("ETag"))
	return serveAdmin(t, server, req)
}

func collectionPutBody(t *testing.T, recipients []string, collection string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true, "collection": json.RawMessage(collection)})
	return string(raw)
}

func adminError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body adminErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an admin error: %s", w.Body.String())
	}
	return body.Error
}

func TestConfigPutIsCheckedAgainstReportedCatalogs(t *testing.T) {
	server, store := testAdminServer(t)
	recipients := twoRecipients(t)
	at := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	seedInstall(t, store, InstallActive, testCatalog(featureExcludeAdd), at)
	seedInstall(t, store, InstallRevoked, nil, at)
	lagging := seedInstall(t, store, InstallActive, nil, at)

	w := putCollection(t, server, collectionPutBody(t, recipients, `{"sources":[{"id":"codex-rollouts"},{"id":"claude-code-transcripts"},{"id":"foo"}]}`))
	if w.Code != http.StatusBadRequest || adminError(t, w) != `collection: sources[2].id "foo" is not in any reporting install's catalog` {
		t.Fatalf("unknown source: %d %s", w.Code, w.Body.String())
	}
	w = putCollection(t, server, collectionPutBody(t, recipients, `{"scrub":{"rule_packs":["pii-core","bar"]}}`))
	if w.Code != http.StatusBadRequest || adminError(t, w) != `collection: scrub.rule_packs[1] "bar" is not in any reporting install's catalog` {
		t.Fatalf("unknown pack: %d %s", w.Code, w.Body.String())
	}
	// authored_yaml is checked as the collection it becomes.
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true, "authored_yaml": "sources: [{id: foo}]"})
	if w = putCollection(t, server, string(raw)); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown source in authored_yaml: %d %s", w.Code, w.Body.String())
	}

	excludeAdd := collectionPutBody(t, recipients, `{"sources":[{"id":"claude-code-transcripts","exclude_add":["client/**"]}]}`)
	w = putCollection(t, server, excludeAdd)
	if w.Code != http.StatusConflict || adminError(t, w) != "collection: sources[].exclude_add needs every active install to report its catalog; 1 of 2 do not yet. Update their shippers first." {
		t.Fatalf("exclude_add with an install not reporting: %d %s", w.Code, w.Body.String())
	}

	// Once every active install reports, it is accepted, and it round-trips through the API.
	reportCatalog(t, store, lagging, testCatalog(), at)
	if w = putCollection(t, server, excludeAdd); w.Code != http.StatusNoContent {
		t.Fatalf("exclude_add with every install reporting: %d %s", w.Code, w.Body.String())
	}
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	var cfg FleetConfig
	_ = json.Unmarshal(w.Body.Bytes(), &cfg)
	if cfg.Collection == nil || !slices.Equal(cfg.Collection.Sources[0].ExcludeAdd, []string{"client/**"}) {
		t.Fatalf("exclude_add did not round-trip: %s", w.Body.String())
	}
}

func TestConfigPutWithNoReportingInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	recipients := twoRecipients(t)
	excludeAdd := collectionPutBody(t, recipients, `{"sources":[{"id":"anything","exclude_add":["client/**"]}]}`)

	// No installs at all: nothing to check against and nobody to fold for.
	if w := putCollection(t, server, excludeAdd); w.Code != http.StatusNoContent {
		t.Fatalf("empty organization: %d %s", w.Code, w.Body.String())
	}

	seedInstall(t, store, InstallActive, nil, time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))
	// A write that does not state a collection keeps the stored one, exclude_add and all, unchecked.
	if w := putCollection(t, server, configJSONWithoutCollection(t, recipients)); w.Code != http.StatusNoContent {
		t.Fatalf("recipients-only write: %d %s", w.Code, w.Body.String())
	}
	// An exclude_add nobody could fold is refused...
	w := putCollection(t, server, excludeAdd)
	if w.Code != http.StatusConflict || !strings.Contains(adminError(t, w), "1 of 1 do not yet") {
		t.Fatalf("exclude_add with no reporting install: %d %s", w.Code, w.Body.String())
	}
	// ...while ids and packs go unchecked, as before catalogs existed.
	if w := putCollection(t, server, collectionPutBody(t, recipients, `{"sources":[{"id":"anything"}],"scrub":{"rule_packs":["any-pack"]}}`)); w.Code != http.StatusNoContent {
		t.Fatalf("unchecked ids: %d %s", w.Code, w.Body.String())
	}
}

func configJSONWithoutCollection(t *testing.T, recipients []string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true})
	return string(raw)
}
