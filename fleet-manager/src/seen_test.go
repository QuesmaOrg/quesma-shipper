package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
)

func shipperFacts(req *http.Request) *http.Request {
	req.Header.Set("X-Shipper-Version", "v0.0.1-108.b09ad82c49d8")
	req.Header.Set("X-Shipper-OS", "macOS 26.5.1")
	req.Header.Set("X-Shipper-Boot", "2026-08-25T10:23:00Z")
	return req
}

func loadSeen(t *testing.T, manager *Manager, installID string) SeenRecord {
	t.Helper()
	rec, _, err := getRecord[SeenRecord](context.Background(), manager.store, seenKey(manager.org, installID))
	if err != nil {
		t.Fatalf("no telemetry for %s: %v", installID, err)
	}
	return rec
}

func TestEnrollmentRecordsClientFacts(t *testing.T) {
	manager, _ := testManager(t)
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	installID, body := enrollBody(t, manager)
	server, _ := NewServer(manager, fakeSigner{}, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", recorder.Code, recorder.Body.String())
	}
	rec := loadSeen(t, manager, installID)
	if rec.OS != "macOS 26.5.1" || rec.ClientVersion != "v0.0.1-108.b09ad82c49d8" || rec.BootedAt != "2026-08-25T10:23:00Z" {
		t.Fatalf("enrollment did not record the facts: %+v", rec)
	}
	if rec.LastConfigAt != nil || rec.LastVendAt != nil {
		t.Fatalf("enrollment claimed a config or vend: %+v", rec)
	}
}

func TestTelemetryTimeoutLeavesEnrollmentRetryable(t *testing.T) {
	manager, store := testManager(t)
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	installID, body := enrollBody(t, manager)
	store.delayGetIn, store.getDelay = "/seen/", 5*seenWriteTimeout
	var logs bytes.Buffer
	server, _ := NewServer(manager, fakeSigner{}, log.New(&logs, "", 0))

	start := time.Now()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body))))
	if elapsed := time.Since(start); recorder.Code != http.StatusOK || elapsed >= 2*seenWriteTimeout {
		t.Fatalf("enroll with slow telemetry: %d after %s", recorder.Code, elapsed)
	}
	if !strings.Contains(logs.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("telemetry timeout was not logged: %q", logs.String())
	}
	if _, err := manager.LoadActiveInstall(context.Background(), installID); err != nil {
		t.Fatalf("install was not committed: %v", err)
	}

	store.getDelay = 0
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("identical retry: %d %s", recorder.Code, recorder.Body.String())
	}
	if rec := loadSeen(t, manager, installID); rec.InstallID != installID {
		t.Fatalf("retry did not record telemetry: %+v", rec)
	}
}

func TestConfigAndVendStampTheirOwnTimes(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config: %d %s", recorder.Code, recorder.Body.String())
	}
	rec := loadSeen(t, manager, installID)
	if rec.LastConfigAt == nil || rec.LastVendAt != nil {
		t.Fatalf("config stamped the wrong field: %+v", rec)
	}
	if rec.OS != "macOS 26.5.1" {
		t.Fatalf("config did not record the facts: %+v", rec)
	}

	// The vend stamp must not erase the config stamp: they answer different questions.
	authorize := authorizeBody(t, manager, installID)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v2/uploads/authorize", authorize, installID, key, uploadAuthorizePreamble)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", recorder.Code, recorder.Body.String())
	}
	rec = loadSeen(t, manager, installID)
	if rec.LastConfigAt == nil || rec.LastVendAt == nil {
		t.Fatalf("vend lost the config stamp: %+v", rec)
	}
}

// Telemetry is never worth failing a shipper's request over.
func TestTelemetryFailureDoesNotFailTheRequest(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	manager.store.(*memoryStore).failPut = true
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config with a broken telemetry store: %d %s", recorder.Code, recorder.Body.String())
	}
}

// A read that fails must not cost the install the timestamp it is not currently stamping.
func TestUnreadableTelemetryIsNotOverwritten(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config: %d", recorder.Code)
	}
	store := manager.store.(*memoryStore)
	store.failGetIn = "/seen/"
	authorize := authorizeBody(t, manager, installID)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v2/uploads/authorize", authorize, installID, key, uploadAuthorizePreamble)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorize with a broken telemetry read: %d", recorder.Code)
	}
	store.failGetIn = ""
	if rec := loadSeen(t, manager, installID); rec.LastConfigAt == nil {
		t.Fatalf("a failed read erased the config stamp: %+v", rec)
	}
}

func TestRepeatedPollsCollapseIntoOneWrite(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	store := manager.store.(*memoryStore)
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}})
	for i := 0; i < 5; i++ {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
		if recorder.Code != http.StatusOK {
			t.Fatalf("config %d: %d", i, recorder.Code)
		}
	}
	if versions := store.versionOf(seenKey(manager.org, installID)); versions > 2 {
		t.Fatalf("five polls within the throttle wrote %d versions", versions)
	}

	// A moved clock is a genuinely new observation and must be recorded.
	base := manager.now
	manager.now = func() time.Time { return base().Add(2 * seenWriteInterval) }
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config after the throttle: %d", recorder.Code)
	}
	rec := loadSeen(t, manager, installID)
	if rec.LastConfigAt == nil || !rec.LastConfigAt.Equal(manager.time()) {
		t.Fatalf("throttle swallowed a later poll: %+v", rec)
	}
}

func TestTouchResetsMismatchedRecord(t *testing.T) {
	manager, _ := testManager(t)
	const installID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	wrongTime := manager.time().Add(-time.Hour)
	raw, _ := encodeRecord(SeenRecord{Schema: schemaVersion, InstallID: "somebody-else", LastVendAt: &wrongTime})
	if err := manager.store.Put(context.Background(), seenKey(manager.org, installID), raw); err != nil {
		t.Fatal(err)
	}
	if err := manager.Touch(context.Background(), installID, seenConfig, clientFacts{OS: "macOS"}); err != nil {
		t.Fatal(err)
	}
	rec := loadSeen(t, manager, installID)
	if rec.InstallID != installID || rec.LastConfigAt == nil || rec.LastVendAt != nil {
		t.Fatalf("mismatched record was not reset: %+v", rec)
	}
}

// Headers are device-chosen text that lands in an administrator's table.
func TestClientFactsAreBounded(t *testing.T) {
	if got := clientFact("  macOS\r\n 26.5.1\t "); got != "macOS 26.5.1" {
		t.Fatalf("control characters survived: %q", got)
	}
	if got := clientFact(strings.Repeat("x", 500)); len([]rune(got)) != 200 {
		t.Fatalf("length = %d, want 200", len([]rune(got)))
	}
}

func TestListSeenSkipsRecordsItCannotUse(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	body, _ := json.Marshal(configRequest{AgentVersion: "v1", ConfigVersions: []int{1}})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, shipperFacts(signedRequest(http.MethodPost, "/v1/config", body, installID, key, "")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config: %d", recorder.Code)
	}
	ctx := context.Background()
	store := manager.store.(*memoryStore)
	_ = store.Put(ctx, seenKey(manager.org, "11111111-1111-1111-1111-111111111111"), []byte("{ not json"))
	stray, _ := encodeRecord(SeenRecord{Schema: schemaVersion, InstallID: "somebody-else"})
	_ = store.Put(ctx, seenKey(manager.org, "22222222-2222-2222-2222-222222222222"), stray)

	records, err := manager.ListSeen(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 || records[0].InstallID != installID {
		t.Fatalf("list returned %+v", records)
	}
}

func TestListSeenReadsTheWholeFleetConcurrently(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()
	want := 40
	for i := 0; i < want; i++ {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		raw, _ := encodeRecord(SeenRecord{Schema: schemaVersion, InstallID: id, OS: "macOS 26.5.1", LastSeenAt: manager.time()})
		if err := manager.store.Put(ctx, seenKey(manager.org, id), raw); err != nil {
			t.Fatal(err)
		}
	}
	store := manager.store.(*memoryStore)
	store.getDelay = 20 * time.Millisecond
	start := time.Now()
	records, err := manager.ListSeen(ctx)
	elapsed := time.Since(start)
	if err != nil || len(records) != want {
		t.Fatalf("list: %d records, %v", len(records), err)
	}
	// Sequentially this is 40 x 20ms; the bound only holds if the reads overlap.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("reading %d records took %s", want, elapsed)
	}
}

func TestSeenEndpointIsSeparateFromInstalls(t *testing.T) {
	server, store := testAdminServer(t)
	const id = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	raw, _ := encodeRecord(SeenRecord{Schema: schemaVersion, InstallID: id, OS: "macOS 26.5.1"})
	if err := store.Put(context.Background(), seenKey("acme", id), raw); err != nil {
		t.Fatal(err)
	}
	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/installs/seen", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("seen: %d %s", w.Code, w.Body.String())
	}
	var records []SeenRecord
	if err := json.Unmarshal(w.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].OS != "macOS 26.5.1" {
		t.Fatalf("seen returned %+v", records)
	}

	// The installs listing must not have grown a telemetry object into a bogus install row.
	w = serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/installs", "", testAdminCredential))
	if w.Code != http.StatusOK {
		t.Fatalf("installs: %d %s", w.Code, w.Body.String())
	}
	var installs []InstallRecord
	if err := json.Unmarshal(w.Body.Bytes(), &installs); err != nil {
		t.Fatal(err)
	}
	if len(installs) != 0 {
		t.Fatalf("telemetry leaked into the installs listing: %+v", installs)
	}
}

func enrollBody(t *testing.T, manager *Manager) (string, []byte) {
	t.Helper()
	identity, _ := age.GenerateX25519Identity()
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	installID := uuid.NewString()
	token, err := manager.CreateGrant(context.Background(), manager.time().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(enrollRequest{Grant: token, InstallID: installID,
		DevicePublicKey: base64.StdEncoding.EncodeToString(public), AgeRecipient: identity.Recipient().String()})
	return installID, body
}

func authorizeBody(t *testing.T, manager *Manager, installID string) []byte {
	t.Helper()
	req := uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: manager.time(), Objects: []uploadObject{{
		ObjectID: "heartbeat", Key: "v1/organization=acme/install=" + installID + "/state/heartbeat.json.age", Size: 19,
		SourceHash: strings.Repeat("a", 64), Metadata: map[string]string{"kind": "heartbeat"}}}}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
