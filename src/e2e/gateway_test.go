// The OTLP gateway end to end: real OTLP fixtures in over HTTP, the real hosted-install pipeline
// in the middle, a control plane that enrolls by grant and a store on httptest at the far end.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/gateway"
	seal "github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const (
	gatewayOrg   = "acme"
	gatewayGrant = "fmi2.acme.00000000-0000-4000-8000-0000000000aa.grant-secret"
)

// grantPlane enrolls any install that presents the grant and authorizes uploads inside that
// install's own root, verifying each request against the device key the install enrolled with.
type grantPlane struct {
	server    *httptest.Server
	store     *fakeStore
	recipient string

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	hostnames map[string]string
	refuse    bool
	issued    int
}

func startGrantPlane(t *testing.T, store *fakeStore, recipient string) *grantPlane {
	t.Helper()
	p := &grantPlane{store: store, recipient: recipient, keys: map[string]ed25519.PublicKey{}, hostnames: map[string]string{}}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *grantPlane) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/v1/enroll":
		var req struct {
			Grant           string `json:"grant"`
			InstallID       string `json:"install_id"`
			DevicePublicKey string `json:"device_public_key"`
			Hostname        string `json:"hostname"`
		}
		if json.Unmarshal(body, &req) != nil || req.Grant != gatewayGrant {
			http.Error(w, "grant refused", http.StatusForbidden)
			return
		}
		pub, err := base64.StdEncoding.DecodeString(req.DevicePublicKey)
		if err != nil {
			http.Error(w, "bad device key", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.keys[req.InstallID], p.hostnames[req.InstallID] = pub, req.Hostname
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"organization": gatewayOrg})
	case "/v1/config":
		// The served document names the organization and its readers, as a fleet manager's does.
		doc := "config_version: 1\norg: " + gatewayOrg + "\nissued_at: " + time.Now().UTC().Format(time.RFC3339) +
			"\nencryption:\n  include_install_recipient: false\n  additional_recipients:\n    - " + p.recipient + "\n"
		_ = json.NewEncoder(w).Encode(map[string]any{"config": []byte(doc), "expires_at": time.Now().Add(24 * time.Hour)})
	case "/v2/uploads/authorize":
		var install string
		header := r.Header.Get("Authorization")
		if _, err := fmt.Sscanf(header, "Shipper-Device org="+gatewayOrg+", install=%36s", &install); err != nil {
			http.Error(w, "not a device signature", http.StatusUnauthorized)
			return
		}
		_, sig64, _ := strings.Cut(header, "sig=")
		sig, _ := base64.StdEncoding.DecodeString(sig64)
		p.mu.Lock()
		pub, refuse := p.keys[install], p.refuse
		seq := p.issued
		p.issued++
		p.mu.Unlock()
		if pub == nil || !ed25519.Verify(pub, append([]byte(vendPreamble), body...), sig) {
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
			return
		}
		if refuse {
			http.Error(w, "told to fail", http.StatusInternalServerError)
			return
		}
		var req authorizeRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tickets := []map[string]any{}
		for i, obj := range req.Objects {
			if !strings.HasPrefix(obj.Key, "v1/organization="+gatewayOrg+"/install="+install+"/") {
				http.Error(w, "key outside the install's root", http.StatusForbidden)
				return
			}
			ticketID := fmt.Sprintf("ticket-%d-%d", seq, i)
			headers := map[string]string{"x-amz-meta-source-hash": obj.SourceHash, "x-amz-meta-ticket-id": ticketID,
				"x-amz-tagging": taggingFor(obj.Key)}
			for name, value := range obj.Metadata {
				headers["x-amz-meta-"+name] = value
			}
			tickets = append(tickets, map[string]any{
				"ticket_id": ticketID, "object_id": obj.ObjectID, "method": "PUT",
				"url":        p.store.server.URL + "/" + vendBucket + "/" + canonicalKeyPath(obj.Key) + "?sig=" + p.store.sign(obj.Key),
				"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano), "required_headers": headers,
				"content_length": obj.Size, "content_length_signed": true,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tickets": tickets})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (p *grantPlane) setRefuse(refuse bool) {
	p.mu.Lock()
	p.refuse = refuse
	p.mu.Unlock()
}

type gatewayWorld struct {
	gw       *gateway.Gateway
	url      string
	token    string
	dataDir  string
	store    *fakeStore
	plane    *grantPlane
	identity *age.X25519Identity
}

func stageGateway(t *testing.T) *gatewayWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The pipeline must not read this process's machine: a HOME with agents in it would be a leak.
	t.Setenv("HOME", filepath.Join(root, "unused-home"))
	identity, err := age.ParseX25519Identity(testAgeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	w := &gatewayWorld{dataDir: filepath.Join(root, "data"), store: startFakeStore(t), identity: identity}
	w.plane = startGrantPlane(t, w.store, identity.Recipient().String())

	shipperConfig := filepath.Join(root, "shipper.yaml")
	writeFile(t, shipperConfig, "config_version: 1\n"+
		"upload_targets:\n  - origin: "+w.store.server.URL+"\n    addressing: path-style\n"+
		"    path_prefix: /"+vendBucket+"\n    allow_loopback_http: true\n")
	var sha string
	w.token, sha = gateway.NewToken(gatewayOrg, "ingest-secret")
	cfg, err := gateway.ParseConfig([]byte("data_dir: " + w.dataDir + "\nshipper_config: " + shipperConfig + "\n" +
		"tenants:\n  - organization: " + gatewayOrg + "\n    fleet_manager: " + w.plane.server.URL + "\n" +
		"    grant: " + gatewayGrant + "\n    token_sha256: [" + sha + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
	w.gw, err = app.NewGateway(cfg, app.NewBuild(), slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.gw.Handler())
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *gatewayWorld) post(t *testing.T, path, contentType string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+w.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (w *gatewayWorld) postFixture(t *testing.T, path, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "otlp", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	contentType := "application/json"
	if strings.HasSuffix(name, ".pb") {
		contentType = "application/x-protobuf"
	}
	if status := w.post(t, path, contentType, raw); status != http.StatusOK {
		t.Fatalf("POST %s %s answered %d", path, name, status)
	}
}

// objects opens every mirror object with the organization's reader identity, keyed by source id.
func (w *gatewayWorld) objects(t *testing.T) map[string][]object {
	t.Helper()
	out := map[string][]object{}
	for _, put := range w.store.mirrorPuts() {
		m, payload, err := seal.Open(put.Body, w.identity)
		if err != nil {
			t.Fatalf("%s: open: %v", put.Key, err)
		}
		if !strings.HasPrefix(put.Key, "v1/organization="+gatewayOrg+"/install="+m.InstallID+"/mirror/source="+m.SourceID+"/") {
			t.Errorf("object %s is outside its install's mirror for %s", put.Key, m.SourceID)
		}
		out[m.SourceID] = append(out[m.SourceID], object{Key: put.Key, Manifest: m, Payload: payload})
	}
	return out
}

func (w *gatewayWorld) spooled(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(w.dataDir, "orgs", "*", "people", "*", "spool", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func promptRequest(email, prompt string) []byte {
	attr := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	raw, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attr("service.name", "claude-code")}},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "com.anthropic.claude_code.events"},
			"logRecords": []any{map[string]any{"timeUnixNano": "1794837864982000000", "attributes": []any{
				attr("event.name", "user_prompt"), attr("user.email", email), attr("session.id", "s-1"), attr("prompt", prompt)}}}}},
	}}})
	return raw
}

func TestGatewayShipsOnePersonsTelemetryThroughTheScrubAndTheSeal(t *testing.T) {
	w := stageGateway(t)
	w.postFixture(t, "/v1/logs", "claude-code-logs.json")
	w.postFixture(t, "/v1/metrics", "claude-code-metrics.pb")
	w.postFixture(t, "/v1/logs", "codex-logs.json")
	if status := w.post(t, "/v1/logs", "application/json", promptRequest("dev@example.com", "deploy with "+seededGitHubToken)); status != http.StatusOK {
		t.Fatalf("the prompt request answered %d", status)
	}
	if failed := w.gw.Flush(context.Background()); failed != 0 {
		t.Fatalf("%d people failed to ship", failed)
	}

	// One email, so Claude Code and Codex are one install, enrolled by the grant under a readable name.
	if len(w.plane.hostnames) != 1 {
		t.Fatalf("enrolled %d installs, want 1: %v", len(w.plane.hostnames), w.plane.hostnames)
	}
	for _, hostname := range w.plane.hostnames {
		if hostname != "otel: dev@example.com" {
			t.Errorf("enrolled as %q", hostname)
		}
	}

	objs := w.objects(t)
	for _, source := range []string{"otel-claude-code", "otel-codex", "otel-identity"} {
		if len(objs[source]) != 1 {
			t.Fatalf("%s: %d objects, want 1 (all sources: %d)", source, len(objs[source]), len(objs))
		}
	}
	claude := objs["otel-claude-code"][0]
	if claude.Manifest.SourceFamily != "otel" || claude.Manifest.OrganizationID != gatewayOrg {
		t.Errorf("unexpected manifest: %+v", claude.Manifest)
	}
	for _, want := range []string{`"output_tokens":537`, `"metric":"claude_code.token.usage"`, `"event.name":"user_prompt"`} {
		if !bytes.Contains(claude.Payload, []byte(want)) {
			t.Errorf("the Claude Code payload lacks %s", want)
		}
	}
	for _, leak := range []string{seededGitHubToken, "dev@example.com", "user.account_uuid"} {
		if bytes.Contains(claude.Payload, []byte(leak)) {
			t.Errorf("the Claude Code payload carries %q", leak)
		}
	}
	if !bytes.Contains(objs["otel-codex"][0].Payload, []byte(`"input_token_count":"18695"`)) {
		t.Error("the Codex payload lacks its completion")
	}
	// The identity object is the one place the claimed email survives.
	if !bytes.Contains(objs["otel-identity"][0].Payload, []byte(`"user.email":"dev@example.com"`)) {
		t.Errorf("the identity payload lacks the email: %s", objs["otel-identity"][0].Payload)
	}

	if left := w.spooled(t); len(left) != 0 {
		t.Errorf("shipped files were not reaped: %v", left)
	}
	before := len(w.store.mirrorPuts())
	if failed := w.gw.Flush(context.Background()); failed != 0 || len(w.store.mirrorPuts()) != before {
		t.Errorf("an idle flush failed %d and wrote %d objects", failed, len(w.store.mirrorPuts())-before)
	}

	// What arrives later is a new object, never a rewrite of a shipped one.
	w.postFixture(t, "/v1/logs", "claude-code-logs.json")
	if failed := w.gw.Flush(context.Background()); failed != 0 {
		t.Fatalf("%d people failed to ship", failed)
	}
	later := w.objects(t)["otel-claude-code"]
	if len(later) != 2 || later[0].Key == later[1].Key {
		t.Errorf("the second batch did not land as its own object: %d objects", len(later))
	}
}

func TestGatewayKeepsTheSpoolUntilTheUploadIsConfirmed(t *testing.T) {
	w := stageGateway(t)
	w.plane.setRefuse(true)
	w.postFixture(t, "/v1/logs", "claude-code-logs.json")
	if failed := w.gw.Flush(context.Background()); failed != 1 {
		t.Fatalf("%d people failed while uploads were refused, want 1", failed)
	}
	if len(w.store.mirrorPuts()) != 0 || len(w.spooled(t)) == 0 {
		t.Fatalf("a refused upload stored %d objects and left %d spool files", len(w.store.mirrorPuts()), len(w.spooled(t)))
	}

	w.plane.setRefuse(false)
	if failed := w.gw.Flush(context.Background()); failed != 0 {
		t.Fatalf("%d people failed after recovery", failed)
	}
	if len(w.objects(t)["otel-claude-code"]) != 1 || len(w.spooled(t)) != 0 {
		t.Errorf("after recovery: %d objects, %d spool files", len(w.objects(t)["otel-claude-code"]), len(w.spooled(t)))
	}
}

func TestGatewayGivesEachEmailItsOwnInstall(t *testing.T) {
	w := stageGateway(t)
	for _, email := range []string{"ada@example.com", "bob@example.com", "ADA@example.com"} {
		if status := w.post(t, "/v1/logs", "application/json", promptRequest(email, "hello")); status != http.StatusOK {
			t.Fatalf("%s answered %d", email, status)
		}
	}
	if failed := w.gw.Flush(context.Background()); failed != 0 {
		t.Fatalf("%d people failed to ship", failed)
	}
	installs := map[string]bool{}
	for _, o := range w.objects(t)["otel-claude-code"] {
		installs[o.Manifest.InstallID] = true
	}
	if len(installs) != 2 || len(w.plane.hostnames) != 2 {
		t.Errorf("%d installs wrote objects and %d enrolled, want 2 and 2", len(installs), len(w.plane.hostnames))
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
