package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
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

// testFeature stands in for a served-document feature; none is defined yet.
const testFeature = "future.feature"

func testCatalog(features ...string) *sourceCatalog {
	return &sourceCatalog{
		Sources: []catalogSource{
			{ID: "claude-code-transcripts", Family: "claude-code", FamilyName: "Claude Code", Enabled: true,
				Roots: []string{"$CLAUDE_CONFIG_DIR", "~/.claude"}, Include: []string{"projects/**/*.jsonl"}, Exclude: []string{"projects/**/*.png"}},
			{ID: "codex-rollouts", Family: "codex", FamilyName: "Codex", Enabled: true, Roots: []string{"~/.codex"}},
		},
		RulePacks: []string{"gitleaks-core", "pii-core"},
		Features:  append([]string{}, features...),
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
	return fetchCollectionWith(t, server, key, installID, catalogJSON(t, catalog))
}

// catalogJSON is a catalog as a request carries it, or no catalog at all.
func catalogJSON(t *testing.T, catalog *sourceCatalog) json.RawMessage {
	t.Helper()
	if catalog == nil {
		return nil
	}
	return mustJSON(t, catalog)
}

// fetchCollectionWith sends the catalog bytes as they are, however malformed.
func fetchCollectionWith(t *testing.T, server *Server, key ed25519.PrivateKey, installID string, catalog json.RawMessage) CollectionConfig {
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

	fetchCollection(t, server, key, installID, testCatalog(testFeature))
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

// A source the build lacks is left out, so the build does not refuse the whole document over it. A
// requested rule pack never is: that would scrub less than asked, and a build lacking one refuses
// the document and keeps its last working one.
func TestServedDocumentLeavesOutSourcesTheBuildLacksAndKeepsEveryRulePack(t *testing.T) {
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
		if served.Scrub == nil || !slices.Equal(served.Scrub.RulePacks, []string{"made-up", "gitleaks-core"}) || !slices.Equal(served.Scrub.SecretKeyNames, []string{"api_key"}) {
			t.Fatalf("a requested rule pack was not served: %+v", served.Scrub)
		}
	}
	if lines := strings.Count(logs.String(), "\n"); lines != 1 || !strings.Contains(logs.String(), `sources[0] "cursor-chats"`) || strings.Contains(logs.String(), "rule_packs") {
		t.Fatalf("want one line naming the source alone, got %q", logs.String())
	}

	// A build without a catalog is served as before.
	served := fetchCollection(t, server, key, installID, nil)
	if len(served.Sources) != 2 || len(served.Scrub.RulePacks) != 2 {
		t.Fatalf("a catalog-less build was filtered: %+v", served)
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
	newer := testCatalog(testFeature)
	newer.Sources[0].Description = "newer words"
	newer.Sources = append(newer.Sources, catalogSource{ID: "aider-history", Family: "aider", FamilyName: "Aider", Enabled: false, Roots: []string{"~"}})
	newer.RulePacks = append(newer.RulePacks, "cloud-keys")
	seedInstall(t, store, InstallActive, older, at(9))
	seedInstall(t, store, InstallActive, newer, at(10))
	seedInstall(t, store, InstallActive, nil, at(11))
	// Neither of these counts, whatever they reported.
	revoked := testCatalog(testFeature)
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
	if !slices.Equal(out.RulePacks, wantPacks) || !slices.Equal(out.Features, []adminNameCount{{testFeature, 1}}) {
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

func putCollectionAcknowledging(t *testing.T, server *Server, recipients []string, collection string, unverified ...string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true,
		"collection": json.RawMessage(collection), "unverified_sources": unverified})
	return putCollection(t, server, string(raw))
}

const unreportedFoo = `collection: sources[0].id "foo" is not reported by any install; send it in unverified_sources to save it anyway`

func TestConfigPutIsCheckedAgainstReportedCatalogs(t *testing.T) {
	server, store := testAdminServer(t)
	recipients := twoRecipients(t)
	at := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	seedInstall(t, store, InstallActive, testCatalog(), at)
	seedInstall(t, store, InstallActive, nil, at)
	revoked := testCatalog()
	revoked.Sources = append(revoked.Sources, catalogSource{ID: "revoked-only", Family: "x", Enabled: true, Roots: []string{"~"}})
	revoked.RulePacks = append(revoked.RulePacks, "revoked-pack")
	seedInstall(t, store, InstallRevoked, revoked, at)

	// Reported IDs pass; a new one nobody reports does not, a revoked install's report included.
	if w := putCollection(t, server, collectionPutBody(t, recipients, `{"sources":[{"id":"codex-rollouts"},{"id":"claude-code-transcripts"}]}`)); w.Code != http.StatusNoContent {
		t.Fatalf("reported sources: %d %s", w.Code, w.Body.String())
	}
	w := putCollection(t, server, collectionPutBody(t, recipients, `{"sources":[{"id":"codex-rollouts"},{"id":"claude-code-transcripts"},{"id":"revoked-only"}]}`))
	if w.Code != http.StatusBadRequest || adminError(t, w) != `collection: sources[2].id "revoked-only" is not reported by any install; send it in unverified_sources to save it anyway` {
		t.Fatalf("unknown source: %d %s", w.Code, w.Body.String())
	}
	// authored_yaml is checked as the collection it becomes.
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true, "authored_yaml": "sources: [{id: foo}]"})
	if w = putCollection(t, server, string(raw)); w.Code != http.StatusBadRequest || adminError(t, w) != unreportedFoo {
		t.Fatalf("unknown source in authored_yaml: %d %s", w.Code, w.Body.String())
	}

	// Configuring a source before its first supporting build checks in: acknowledged, it saves,
	// and the acknowledgement itself is not stored.
	if w = putCollectionAcknowledging(t, server, recipients, `{"sources":[{"id":"foo","enabled":true}]}`, "foo", "claude-code-transcripts"); w.Code != http.StatusNoContent {
		t.Fatalf("acknowledged source: %d %s", w.Code, w.Body.String())
	}
	if raw, _, _ := store.Get(context.Background(), configKey("acme")); bytes.Contains(raw, []byte("unverified")) {
		t.Fatalf("unverified_sources was stored: %s", raw)
	}
	// Once stored, the ID is kept on later writes without acknowledging it again -- it may be a
	// build that does not report a catalog that uses it.
	if w = putCollection(t, server, collectionPutBody(t, recipients, `{"mode":{"schedule":"30m"},"sources":[{"id":"foo","enabled":false}]}`)); w.Code != http.StatusNoContent {
		t.Fatalf("stored source: %d %s", w.Code, w.Body.String())
	}
	if w = putCollection(t, server, collectionPutBody(t, recipients, `{"sources":[{"id":"foo"},{"id":"bar"}]}`)); w.Code != http.StatusBadRequest || !strings.Contains(adminError(t, w), `sources[1].id "bar"`) {
		t.Fatalf("a second unknown source: %d %s", w.Code, w.Body.String())
	}

	// Rule packs stay strict while installs report, and acknowledging does not cover them.
	w = putCollectionAcknowledging(t, server, recipients, `{"scrub":{"rule_packs":["pii-core","revoked-pack"]}}`, "revoked-pack")
	if w.Code != http.StatusBadRequest || adminError(t, w) != `collection: scrub.rule_packs[1] "revoked-pack" is not in any reporting install's catalog` {
		t.Fatalf("unknown pack: %d %s", w.Code, w.Body.String())
	}
}

func TestConfigPutWithNoReportingInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	recipients := twoRecipients(t)
	for _, id := range []string{"foo", "baz"} {
		if id == "baz" {
			seedInstall(t, store, InstallActive, nil, time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))
		}
		// Nothing reports, so only a stored or acknowledged ID passes, with or without installs...
		want := `collection: sources[1].id "` + id + `" is not reported by any install; send it in unverified_sources to save it anyway`
		collection := `{"sources":[{"id":"foo"},{"id":"` + id + `"}],"scrub":{"rule_packs":["any-pack"]}}`
		if id == "foo" {
			want = unreportedFoo
			collection = `{"sources":[{"id":"foo"}],"scrub":{"rule_packs":["any-pack"]}}`
		}
		if w := putCollection(t, server, collectionPutBody(t, recipients, collection)); w.Code != http.StatusBadRequest || adminError(t, w) != want {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
		// ...while packs go unchecked, as before catalogs existed.
		if w := putCollectionAcknowledging(t, server, recipients, collection, id); w.Code != http.StatusNoContent {
			t.Fatalf("%s acknowledged: %d %s", id, w.Code, w.Body.String())
		}
	}
	// A write that does not state a collection keeps the stored one, unchecked.
	if w := putCollection(t, server, configJSONWithoutCollection(t, recipients)); w.Code != http.StatusNoContent {
		t.Fatalf("recipients-only write: %d %s", w.Code, w.Body.String())
	}
}

func configJSONWithoutCollection(t *testing.T, recipients []string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"age_recipients": recipients, "include_install_recipient": true})
	return string(raw)
}

// A catalog that is not what the schema requires is treated as absent, never as a catalog with no
// sources: that would leave every source override out of the document, and a source the
// administrator turned off would come back on under the build's compiled defaults. A wrong type
// does not fail the request either. Unknown fields and explicit empty lists are still accepted.
func TestMalformedCatalogIsServedAsNoCatalog(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	server.logger = log.New(io.Discard, "", 0)
	setCollection(t, manager, &CollectionConfig{Sources: []CollectionSource{
		{ID: "claude-code-transcripts", Enabled: ptr(false), Exclude: []string{"secret/**"}},
	}})
	want := fetchCollection(t, server, key, installID, nil)
	if source := servedSource(t, want, "claude-code-transcripts"); source.Enabled == nil || *source.Enabled {
		t.Fatalf("the override is not served without a catalog: %+v", source)
	}
	source := `{"id":"claude-code-transcripts","family":"claude-code","enabled":true,"roots":["~/.claude"]}`
	for _, catalog := range []string{
		`{}`,
		`{"sources":null,"rule_packs":[],"features":[]}`,
		`{"sources":[],"rule_packs":[]}`,
		`{"sources":[` + source + `],"rule_packs":null,"features":[]}`,
		`{"sources":"claude-code-transcripts","rule_packs":[],"features":[]}`,
		`{"sources":[` + source + `],"rule_packs":[7],"features":[]}`,
		`{"sources":[` + source + `],"rule_packs":[""],"features":[]}`,
		`{"sources":[null],"rule_packs":[],"features":[]}`,
		`{"sources":[{"id":"claude-code-transcripts","family":"claude-code","roots":["~/.claude"]}],"rule_packs":[],"features":[]}`,
		`{"sources":[{"id":"claude-code-transcripts","family":"claude-code","enabled":true,"roots":[null]}],"rule_packs":[],"features":[]}`,
		`{"sources":[{"id":"claude-code-transcripts","family":"claude-code","enabled":true,"roots":[],"max_file_bytes":"big"}],"rule_packs":[],"features":[]}`,
		`[]`,
		`5`,
	} {
		served := fetchCollectionWith(t, server, key, installID, json.RawMessage(catalog))
		if !bytes.Equal(mustJSON(t, served), mustJSON(t, want)) {
			t.Fatalf("catalog %s was acted on: served %+v", catalog, served)
		}
		if got := loadSeen(t, manager, installID).CatalogDigest; got != "" {
			t.Fatalf("catalog %s was recorded as %q", catalog, got)
		}
	}

	// A field this service does not know yet is ignored, and explicit empty lists are a catalog.
	future := `{"sources":[{"id":"claude-code-transcripts","family":"claude-code","enabled":true,"roots":[],"next":1}],"rule_packs":[],"features":[],"next":{}}`
	served := fetchCollectionWith(t, server, key, installID, json.RawMessage(future))
	if source := servedSource(t, served, "claude-code-transcripts"); source.Enabled == nil || *source.Enabled {
		t.Fatalf("a catalog with unknown fields lost the override: %+v", source)
	}
	if loadSeen(t, manager, installID).CatalogDigest == "" {
		t.Fatal("a catalog with unknown fields was not recorded")
	}
	served = fetchCollectionWith(t, server, key, installID, json.RawMessage(`{"sources":[],"rule_packs":[],"features":[]}`))
	if len(served.Sources) != 0 || loadSeen(t, manager, installID).CatalogDigest == "" {
		t.Fatalf("a build reporting no sources: %+v", served.Sources)
	}
}

// oversizedCatalog fits a config request but not, encoded as a record, a state object.
func oversizedCatalog() *sourceCatalog {
	catalog := testCatalog()
	catalog.Sources[0].Roots = make([]string, 330_000)
	return catalog
}

// One install's catalog must not be able to stop the organization's configuration: a record the
// stores could not read back is never written, and one already stored counts as no catalog.
func TestOversizedCatalogIsNotStored(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	var logs bytes.Buffer
	server.logger = log.New(&logs, "", 0)
	catalog := oversizedCatalog()
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}, Catalog: catalogJSON(t, catalog)})
	if len(body) > requestLimit {
		t.Fatalf("the request is %d bytes, above the request limit", len(body))
	}
	if record, _ := encodeRecord(CatalogRecord{Schema: schemaVersion, Catalog: *catalog}); len(record) <= stateObjectLimit {
		t.Fatalf("the record is %d bytes, within the state object limit", len(record))
	}
	fetchCollection(t, server, key, installID, catalog)
	if got := loadSeen(t, manager, installID).CatalogDigest; got != "" {
		t.Fatalf("an oversized catalog was recorded as %q", got)
	}
	if objects, _ := manager.store.List(context.Background(), controlPrefix("acme")+"catalogs/"); len(objects) != 0 {
		t.Fatalf("an oversized catalog was stored: %+v", objects)
	}
	if !strings.Contains(logs.String(), "ignored its catalog") {
		t.Fatalf("not logged: %q", logs.String())
	}
}

func TestStoredOversizedCatalogCountsAsNone(t *testing.T) {
	server, store := testAdminServer(t)
	at := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	seedInstall(t, store, InstallActive, testCatalog(), at)
	// Written before records were bounded: the store holds it, and no provider reads it.
	id := seedInstall(t, store, InstallActive, nil, at)
	raw, _ := json.MarshalIndent(CatalogRecord{Schema: schemaVersion, Catalog: *oversizedCatalog()}, "", "  ")
	_ = store.Create(context.Background(), catalogKey("acme", "oversized"), raw)
	seen := SeenRecord{Schema: schemaVersion, InstallID: id, LastConfigAt: &at, LastSeenAt: at, CatalogDigest: "oversized"}
	record, _ := encodeRecord(seen)
	if err := store.Put(context.Background(), seenKey("acme", id), record); err != nil {
		t.Fatal(err)
	}

	if out, _ := listSources(t, server); out.Installs != (adminSourcesInstalls{Active: 2, Reporting: 1}) {
		t.Fatalf("installs %+v", out.Installs)
	}
	if w := putCollection(t, server, collectionPutBody(t, twoRecipients(t), `{"sources":[{"id":"codex-rollouts"}]}`)); w.Code != http.StatusNoContent {
		t.Fatalf("collection update: %d %s", w.Code, w.Body.String())
	}
}

// The admin UI sends the collection with every save. A pack saved before any catalog arrived, or
// for a build that does not report one, stays saveable once other builds report: rotating
// recipients keeps it, while adding a pack no build has is still refused.
func TestStoredRulePackIsNotRefusedAgain(t *testing.T) {
	server, store := testAdminServer(t)
	collection := `{"scrub":{"rule_packs":["gitleaks-core","private-pack"]}}`
	if w := putCollection(t, server, collectionPutBody(t, twoRecipients(t), collection)); w.Code != http.StatusNoContent {
		t.Fatalf("before any catalog: %d %s", w.Code, w.Body.String())
	}
	seedInstall(t, store, InstallActive, testCatalog(), time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))

	if w := putCollection(t, server, collectionPutBody(t, twoRecipients(t), collection)); w.Code != http.StatusNoContent {
		t.Fatalf("recipient rotation: %d %s", w.Code, w.Body.String())
	}
	raw, _, _ := store.Get(context.Background(), configKey("acme"))
	if !bytes.Contains(raw, []byte("private-pack")) {
		t.Fatalf("the stored pack was lost: %s", raw)
	}
	added := `{"scrub":{"rule_packs":["gitleaks-core","private-pack","other-pack"]}}`
	w := putCollection(t, server, collectionPutBody(t, twoRecipients(t), added))
	if w.Code != http.StatusBadRequest || adminError(t, w) != `collection: scrub.rule_packs[2] "other-pack" is not in any reporting install's catalog` {
		t.Fatalf("a new unsupported pack: %d %s", w.Code, w.Body.String())
	}
}
