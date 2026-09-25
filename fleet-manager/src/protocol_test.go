package main

import (
	"io/fs"
	"path"

	protocol "github.com/QuesmaOrg/shipper-protocol"

	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The wire contract comes from the published module, not from a sibling directory. These are
// contract tests: they should assert against the authority, and not against wherever this
// repository happens to sit relative to a checkout of it.
func readProtocol(parts ...string) ([]byte, error) {
	return fs.ReadFile(protocol.FS, path.Join(parts...))
}

type fakeSigner struct{}

func (fakeSigner) Authorize(_ context.Context, _ InstallScope, batch UploadBatch) (TicketBatch, error) {
	out := TicketBatch{Tickets: make([]uploadTicket, 0, len(batch.Objects))}
	for _, object := range batch.Objects {
		headers := map[string]string{}
		for name, value := range object.Metadata {
			headers["x-amz-meta-"+name] = value
		}
		if object.Tagging != "" {
			headers["x-amz-tagging"] = object.Tagging
		}
		out.Tickets = append(out.Tickets, uploadTicket{TicketID: object.TicketID, ObjectID: object.ObjectID,
			Method: http.MethodPut, URL: "https://objects.example.invalid/" + object.Key + "?sig=secret", ExpiresAt: batch.ExpiresAt,
			RequiredHeaders: headers, ContentLength: object.Size, ContentLengthSigned: true})
	}
	return out, nil
}

func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := readProtocol("schemas", name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}
func validateSchema(t *testing.T, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := compileSchema(t, name).Validate(doc); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, raw)
	}
}

func TestWireStructsMatchSchemas(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	validateSchema(t, "enroll-request.schema.json", enrollRequest{Invite: "fmi2.acme.id.secret", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		DevicePublicKey: "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=", AgeRecipient: "age1cpx4grz9j4fkn36cfurggwcg4l0da5fyqadl8fwagtcwy55gt44qlclfa5"})
	validateSchema(t, "enroll-response.schema.json", enrollResponse{Organization: "acme"})
	validateSchema(t, "config-request.schema.json", configRequest{AgentVersion: "test", ConfigVersions: []int{1}})
	validateSchema(t, "config-response.schema.json", configResponse{Config: []byte("config_version: 1\n"), ExpiresAt: now})
	validateSchema(t, "v2/uploads-authorize-request.schema.json", uploadAuthorizeRequest{WriterID: "8403c1de-6940-4e35-a19b-5c91c45fc379", IssuedAt: now,
		Objects: []uploadObject{{ObjectID: "heartbeat", Key: "v1/organization=acme/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301/state/heartbeat.json.age",
			Size: 10, SourceHash: strings.Repeat("a", 64), Metadata: map[string]string{"kind": "heartbeat"}}}})
	validateSchema(t, "v2/uploads-authorize-response.schema.json", uploadAuthorizeResponse{
		Tickets: []uploadTicket{{TicketID: "8403c1de-6940-4e35-a19b-5c91c45fc379", ObjectID: "heartbeat", AlreadyPresent: true}}})
}

func TestUploadFixturesUseTheExistingContract(t *testing.T) {
	install := InstallRecord{Organization: "acme", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301"}
	for _, name := range []string{"request.json", "request-batch.json", "request-heartbeat.json"} {
		raw, err := readProtocol("fixtures/v2/uploads-authorize", name)
		if err != nil {
			t.Fatal(err)
		}
		var req uploadAuthorizeRequest
		if err := strictDecode(raw, &req); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if _, err := validateUploadRequest(req, install, req.IssuedAt); err != nil {
			t.Fatalf("%s validate: %v", name, err)
		}
	}
	for _, name := range []string{"bad-request-unknown-field.json", "bad-request-missing-source-hash.json", "bad-request-size-zero.json"} {
		raw, _ := readProtocol("fixtures/v2/uploads-authorize", name)
		var req uploadAuthorizeRequest
		if err := strictDecode(raw, &req); err == nil {
			if _, err = validateUploadRequest(req, install, req.IssuedAt); err == nil {
				t.Fatalf("accepted %s", name)
			}
		}
	}
}

func enrolledServer(t *testing.T) (*Server, *Manager, ed25519.PrivateKey, string) {
	t.Helper()
	manager, _ := testManager(t)
	identity, _ := age.GenerateX25519Identity()
	if err := manager.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	installID := uuid.NewString()
	token, _ := manager.CreateGrant(context.Background(), manager.time().Add(time.Hour))
	req := enrollRequest{Grant: token, InstallID: installID, DevicePublicKey: base64.StdEncoding.EncodeToString(public), AgeRecipient: identity.Recipient().String()}
	body, _ := json.Marshal(req)
	server, _ := NewServer(manager, fakeSigner{}, nil)
	attachTelemetryProxy(t, server)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", recorder.Code, recorder.Body.String())
	}
	return server, manager, private, installID
}

func TestHealthBeforeInitialization(t *testing.T) {
	manager, _ := testManager(t)
	server, err := NewServer(manager, fakeSigner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/health"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.String() != "ok\n" {
			t.Fatalf("%s before init: %d %q", path, recorder.Code, recorder.Body.String())
		}
	}
}

func signedRequestForOrg(method, path string, body []byte, org, installID string, key ed25519.PrivateKey, preamble string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	signature := ed25519.Sign(key, append([]byte(preamble), body...))
	auth := "Shipper-Device "
	if org != "" {
		auth += "org=" + org + ", "
	}
	req.Header.Set("Authorization", auth+"install="+installID+", sig="+base64.StdEncoding.EncodeToString(signature))
	return req
}

func signedRequest(method, path string, body []byte, installID string, key ed25519.PrivateKey, preamble string) *http.Request {
	return signedRequestForOrg(method, path, body, "acme", installID, key, preamble)
}

func TestMultiTenantDeviceAuthenticationUsesOrganizationLocator(t *testing.T) {
	manager, _ := testMultiTenantManager(t)
	acme, _ := manager.ForOrganization("acme")
	if err := acme.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	identity, _ := age.GenerateX25519Identity()
	token, _ := acme.CreateGrant(context.Background(), manager.time().Add(time.Hour))
	installID := uuid.NewString()
	enroll := enrollRequest{Grant: token, InstallID: installID, DevicePublicKey: base64.StdEncoding.EncodeToString(public), AgeRecipient: identity.Recipient().String()}
	body, _ := json.Marshal(enroll)
	server, _ := NewServer(manager, fakeSigner{}, nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}

	configBody := []byte(`{"agent_version":"test","config_versions":[1]}`)
	for _, tc := range []struct {
		name string
		org  string
		want int
	}{{"matching", "acme", http.StatusOK}, {"missing", "", http.StatusUnauthorized}, {"other", "other", http.StatusUnauthorized}, {"invalid", "../acme", http.StatusUnauthorized}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, signedRequestForOrg(http.MethodPost, "/v1/config", configBody, tc.org, installID, private, ""))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	upload := uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: manager.time(), Objects: []uploadObject{{
		ObjectID: "heartbeat", Key: "v1/organization=other/install=" + installID + "/state/heartbeat.json.age", Size: 19,
		SourceHash: strings.Repeat("a", 64), Metadata: map[string]string{"kind": "heartbeat"}}}}
	uploadBody, _ := json.Marshal(upload)
	w = httptest.NewRecorder()
	server.Handler().ServeHTTP(w, signedRequestForOrg(http.MethodPost, "/v2/uploads/authorize", uploadBody, "acme", installID, private, uploadAuthorizePreamble))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-organization upload = %d: %s", w.Code, w.Body.String())
	}
}

func TestConfigAndRevocationAreImmediatelyVisible(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	body := []byte(`{"agent_version":"test","config_versions":[1]}`)
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, signedRequest(http.MethodPost, "/v1/config", body, installID, key, ""))
	if first.Code != http.StatusOK {
		t.Fatalf("config: %d %s", first.Code, first.Body.String())
	}
	if err := manager.RevokeInstall(context.Background(), installID); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, signedRequest(http.MethodPost, "/v1/config", body, installID, key, ""))
	if second.Code != http.StatusForbidden {
		t.Fatalf("revoked config = %d", second.Code)
	}
}

// A configuration that says nothing about Quesma is sealed to Quesma's recipient as well, exactly
// once, and only an explicit false leaves it out. The absent and explicit-true cases must render
// identically.
func TestRenderedConfigDefaultsQuesmaETLOnAndCanDisableIt(t *testing.T) {
	if _, err := age.ParseX25519Recipient(quesmaETLAgeRecipient); err != nil {
		t.Fatalf("compiled Quesma ETL recipient: %v", err)
	}
	recipients := testAgeRecipients(t, 2)
	for _, allow := range []*bool{nil, ptr(true)} {
		on := renderConfig(FleetConfig{Organization: "acme", AgeRecipients: recipients, AllowQuesmaETL: allow})
		if strings.Count(on, quesmaETLAgeRecipient) != 1 {
			t.Fatalf("allow_quesma_etl %v must contain the Quesma ETL recipient once:\n%s", allow, on)
		}
	}
	disabled := renderConfig(FleetConfig{Organization: "acme", AgeRecipients: recipients, AllowQuesmaETL: ptr(false)})
	if strings.Contains(disabled, quesmaETLAgeRecipient) {
		t.Fatalf("disabled config contains the Quesma ETL recipient:\n%s", disabled)
	}
}

func TestUploadAuthorizationRefusesReplayAndControlKeys(t *testing.T) {
	server, _, key, installID := enrolledServer(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	req := uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: now, Objects: []uploadObject{{ObjectID: "x", Key: "v1/organization=acme/control/config.json", Size: 1,
		SourceHash: strings.Repeat("a", 64), Metadata: map[string]string{"kind": "heartbeat"}}}}
	body, _ := json.Marshal(req)
	wrong := httptest.NewRecorder()
	server.Handler().ServeHTTP(wrong, signedRequest(http.MethodPost, "/v2/uploads/authorize", body, installID, key, ""))
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("v1 replay = %d", wrong.Code)
	}
	refused := httptest.NewRecorder()
	server.Handler().ServeHTTP(refused, signedRequest(http.MethodPost, "/v2/uploads/authorize", body, installID, key, uploadAuthorizePreamble))
	if refused.Code != http.StatusBadRequest || refused.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("control key = %d, cache=%q", refused.Code, refused.Header().Get("Cache-Control"))
	}
}

func TestUploadAuthorizationReturnsOneExactTicket(t *testing.T) {
	server, _, key, installID := enrolledServer(t)
	req := uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC), Objects: []uploadObject{{
		ObjectID: "heartbeat", Key: "v1/organization=acme/install=" + installID + "/state/heartbeat.json.age", Size: 19,
		SourceHash: strings.Repeat("a", 64), Metadata: map[string]string{"kind": "heartbeat"}}}}
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, signedRequest(http.MethodPost, "/v2/uploads/authorize", body, installID, key, uploadAuthorizePreamble))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authorize: %d %s", recorder.Code, recorder.Body.String())
	}
	var response uploadAuthorizeResponse
	if err := strictDecode(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tickets) != 1 || !strings.Contains(response.Tickets[0].URL, "/"+req.Objects[0].Key+"?") {
		t.Fatalf("ticket does not name exact key: %#v", response.Tickets)
	}
}

type serverAuthFixture struct {
	Organization    string               `json:"organization"`
	InstallID       string               `json:"install_id"`
	DevicePublicKey string               `json:"device_public_key"`
	ServerTime      time.Time            `json:"server_time"`
	Config          serverAuthExchange   `json:"config"`
	Authorize       serverAuthExchange   `json:"authorize"`
	Rejected        []serverAuthExchange `json:"rejected"`
}

type serverAuthExchange struct {
	Name          string `json:"name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Body          string `json:"body"`
	Authorization string `json:"authorization"`
}

func TestServerConsumesAuthorizationHeaderGoldens(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			raw, err := readProtocol("fixtures", version, "auth", "headers.json")
			if err != nil {
				t.Fatal(err)
			}
			var fx serverAuthFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			manager, store := testMultiTenantManager(t)
			if !fx.ServerTime.IsZero() {
				manager.now = func() time.Time { return fx.ServerTime }
			}
			scoped, _ := manager.ForOrganization(fx.Organization)
			if err := scoped.Init(context.Background(), FleetConfig{AgeRecipients: testAgeRecipients(t, 2), IncludeInstallRecipient: true}); err != nil {
				t.Fatal(err)
			}
			now := manager.time()
			rec := InstallRecord{Schema: schemaVersion, Organization: fx.Organization, InstallID: fx.InstallID,
				DevicePublicKey: fx.DevicePublicKey, Status: InstallActive, CreatedAt: now, UpdatedAt: now}
			if err := createRecord(context.Background(), store, installKey(fx.Organization, fx.InstallID), rec); err != nil {
				t.Fatal(err)
			}
			server, _ := NewServer(manager, fakeSigner{}, nil)
			valid := fx.Config
			if version == "v2" {
				valid = fx.Authorize
			}
			method, path := valid.Method, valid.Path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = "/v1/config"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(valid.Body))
			request.Header.Set("Authorization", valid.Authorization)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, request)
			if w.Code != http.StatusOK {
				t.Fatalf("valid golden = %d: %s", w.Code, w.Body.String())
			}
			for _, rejected := range fx.Rejected {
				t.Run(rejected.Name, func(t *testing.T) {
					method, path := rejected.Method, rejected.Path
					if method == "" {
						method = http.MethodPost
					}
					if path == "" {
						path = "/v1/config"
					}
					request := httptest.NewRequest(method, path, strings.NewReader(rejected.Body))
					request.Header.Set("Authorization", rejected.Authorization)
					w := httptest.NewRecorder()
					server.Handler().ServeHTTP(w, request)
					if w.Code == http.StatusOK {
						t.Fatalf("rejected golden was accepted: %s", w.Body.String())
					}
				})
			}
		})
	}
}

func TestUploadAuthorizationAnswersAlreadyPresent(t *testing.T) {
	server, manager, key, installID := enrolledServer(t)
	store := manager.store.(*memoryStore)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("a", 64)
	mirrorKey := func(c string) string {
		return "v1/organization=acme/install=" + installID + "/mirror/source=claude/" + strings.Repeat(c, 64) + ".age"
	}
	object := func(id, objectKey string) uploadObject {
		return uploadObject{ObjectID: id, Key: objectKey, Size: 10, SourceHash: hash, Metadata: map[string]string{
			"manifest-version": "1", "source-id": "claude", "shipped-hash": strings.Repeat("d", 64), "artifact-class": "trajectory"}}
	}
	store.setSourceHash(mirrorKey("b"), hash)
	store.setSourceHash(mirrorKey("e"), strings.Repeat("f", 64))

	authorize := func(req uploadAuthorizeRequest) *httptest.ResponseRecorder {
		body, _ := json.Marshal(req)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, signedRequest(http.MethodPost, "/v2/uploads/authorize", body, installID, key, uploadAuthorizePreamble))
		if recorder.Code != http.StatusOK {
			t.Fatalf("authorize: %d %s", recorder.Code, recorder.Body.String())
		}
		return recorder
	}

	// Only an exact source-hash match settles; a grown file under the same key re-uploads.
	recorder := authorize(uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: now,
		Objects: []uploadObject{object("stored", mirrorKey("b")), object("fresh", mirrorKey("c")), object("grown", mirrorKey("e"))}})
	var response uploadAuthorizeResponse
	if err := strictDecode(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	byID := map[string]uploadTicket{}
	for _, ticket := range response.Tickets {
		byID[ticket.ObjectID] = ticket
	}
	if len(byID) != 3 || !byID["stored"].AlreadyPresent || byID["fresh"].AlreadyPresent || byID["grown"].AlreadyPresent {
		t.Fatalf("tickets: %#v", response.Tickets)
	}
	if byID["fresh"].URL == "" || byID["grown"].URL == "" {
		t.Fatalf("unsettled objects carry no ticket: %#v", response.Tickets)
	}
	var raw struct {
		Tickets []map[string]any `json:"tickets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, entry := range raw.Tickets {
		if entry["object_id"] == "stored" && len(entry) != 3 {
			t.Fatalf("the already-present answer carries more than its closed shape: %v", entry)
		}
	}

	// A batch the store already holds whole answers without the signer.
	whole := authorize(uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: now,
		Objects: []uploadObject{object("stored", mirrorKey("b"))}})
	if err := strictDecode(whole.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tickets) != 1 || !response.Tickets[0].AlreadyPresent {
		t.Fatalf("whole batch: %#v", response.Tickets)
	}

	// A state object is never asked about, even one the store holds under the offered hash: the
	// heartbeat is rewritten every tick, so the answer is a plain ticket and the store sees nothing.
	heartbeatKey := "v1/organization=acme/install=" + installID + "/state/heartbeat.json.age"
	store.setSourceHash(heartbeatKey, hash)
	store.mu.Lock()
	store.hashReads = 0
	store.mu.Unlock()
	beat := authorize(uploadAuthorizeRequest{WriterID: uuid.NewString(), IssuedAt: now,
		Objects: []uploadObject{{ObjectID: "heartbeat", Key: heartbeatKey, Size: 10, SourceHash: hash, Metadata: map[string]string{"kind": "heartbeat"}}}})
	var plain uploadAuthorizeResponse
	if err := strictDecode(beat.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if len(plain.Tickets) != 1 || plain.Tickets[0].AlreadyPresent || plain.Tickets[0].URL == "" {
		t.Fatalf("the heartbeat was not handed a plain ticket: %#v", plain.Tickets)
	}
	if store.hashReads != 0 {
		t.Fatalf("a heartbeat authorization probed the store %d times", store.hashReads)
	}
}

type failingHashStore struct{ ObjectStore }

func (failingHashStore) SourceHash(context.Context, string) (string, error) {
	return "", errorsNew("store answered 500 for https://bucket.example/k?sig=secret")
}

// A probe failure is advisory (the object is authorized as new) and the log never carries URLs.
func TestProbeFailureAuthorizesAsNewAndScrubsTheLog(t *testing.T) {
	var buf bytes.Buffer
	objects := []UploadObjectRequest{{ObjectID: "a", Key: "k", Mirror: true, Metadata: map[string]string{"source-hash": strings.Repeat("a", 64)}}}
	needed, settled := splitAlreadyStored(context.Background(), failingHashStore{newMemoryStore()}, log.New(&buf, "", 0), "install", objects)
	if len(needed) != 1 || settled != nil {
		t.Fatalf("needed %d, settled %v", len(needed), settled)
	}
	if !strings.Contains(buf.String(), "1 of 1") || strings.Contains(buf.String(), "sig=") {
		t.Fatalf("probe log: %s", buf.String())
	}
}

// A ticket names an origin the client will PUT to, so what schemes are admissible is a boundary,
// not a detail: only https, plus loopback http for a store on the same machine.
func TestTicketOriginAdmitsHTTPSAndLoopbackHTTPOnly(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://objects.example.invalid/v1/o", true},
		{"http://127.0.0.1:9100/archive/v1/o", true},
		{"http://localhost:9100/archive/v1/o", true},
		{"http://[::1]:9100/archive/v1/o", true},
		{"http://objects.example.invalid/v1/o", false},
		{"http://169.254.169.254/v1/o", false},
		{"ftp://127.0.0.1/v1/o", false},
		{"https://user:pass@objects.example.invalid/v1/o", false},
	} {
		issued := time.Now().UTC()
		object := UploadObjectRequest{ObjectID: "o1", TicketID: "t1", Key: "v1/o", Size: 7,
			Metadata: map[string]string{"source-hash": "sha256:abc"}}
		batch := UploadBatch{IssuedAt: issued, ExpiresAt: issued.Add(time.Minute), Objects: []UploadObjectRequest{object}}
		tickets := TicketBatch{Tickets: []uploadTicket{{TicketID: object.TicketID, ObjectID: object.ObjectID,
			Method: http.MethodPut, URL: tc.url, ExpiresAt: batch.ExpiresAt, ContentLength: object.Size,
			ContentLengthSigned: true, RequiredHeaders: map[string]string{"x-amz-meta-source-hash": "sha256:abc"}}}}
		err := validateTicketBatch(batch, tickets)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.url, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted", tc.url)
		}
	}
}
