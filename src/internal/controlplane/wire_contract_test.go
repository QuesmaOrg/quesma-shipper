// Wire-contract tests: this module's copy of the protocol structs against the independently
// versioned protocol module. The control plane runs the same checks against its copy, which keeps
// the deliberately duplicated struct sets equal without coupling their implementations.
package controlplane_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	pathpkg "path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	protocol "github.com/QuesmaOrg/shipper-protocol"
)

// wireSchemas is the complete set, versioned subdirectories included. A schema added to
// the shipper-protocol module without a row here (or vice versa) fails TestWireSchemasComplete.
var wireSchemas = []string{
	"enroll-request.schema.json",
	"enroll-response.schema.json",
	"config-request.schema.json",
	"config-response.schema.json",
	"v2/uploads-authorize-request.schema.json",
	"v2/uploads-authorize-response.schema.json",
}

func compileWireSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, pathpkg.Join("schemas", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, doc); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	s, err := c.Compile(name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

func TestWireSchemasComplete(t *testing.T) {
	entries, err := fs.Glob(protocol.FS, "schemas/*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	versioned, err := fs.Glob(protocol.FS, "schemas/*/*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, versioned...)
	if len(entries) != len(wireSchemas) {
		t.Errorf("shipper-protocol embeds %d schemas, wireSchemas lists %d — keep them in step", len(entries), len(wireSchemas))
	}
	for _, name := range wireSchemas {
		compileWireSchema(t, name)
	}
}

// Fixture values, shared with the shipper-protocol v1 fixtures. The auth fixture pins the same
// identity; TestAuthHeaderGolden asserts the seed reproduces this public key.
const (
	fixtureInstallID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	fixtureDeviceKey = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	fixtureRecipient = "age1cpx4grz9j4fkn36cfurggwcg4l0da5fyqadl8fwagtcwy55gt44qlclfa5"
	fixtureKeyRoot   = "v1/organization=acme/install=" + fixtureInstallID

	fixtureWriterID     = "8403c1de-6940-4e35-a19b-5c91c45fc379"
	fixtureTicketID     = "b1bd1a73-f16d-4a51-aac6-29f1f48b0658"
	fixtureSourceHash   = "94f4d055cd323cbae40c74b5ed35df51b8fb4f362d7f43acb9b85cfbca5f2cb8"
	fixtureShippedHash  = "f70fd6b3501a3ff995e69e00ad1b1813ac296cf0768d30445549f8aefbd598b7"
	fixtureMirrorKey    = fixtureKeyRoot + "/mirror/source=claude-code-transcripts/f91631a3882c7956a9d2061b96ea38fd75bc77549b85cab26c7b2b63db21ed73.age"
	fixtureHeartbeatKey = fixtureKeyRoot + "/state/heartbeat.json.age"
)

// Struct → schema: a fully-populated instance of every wire struct must validate. With
// additionalProperties: false throughout, a struct field the spec does not know fails here.
func TestStructsMatchSchemas(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		v      any
	}{
		{"enroll request, invite", "enroll-request.schema.json", controlplane.EnrollRequest{
			Invite: "9f8d2c1a-opaque-invite-token", InstallID: fixtureInstallID,
			DevicePublicKey: fixtureDeviceKey, AgeRecipient: fixtureRecipient,
			Hostname: "dev-laptop", Platform: "darwin/arm64",
		}},
		{"enroll request, grant", "enroll-request.schema.json", controlplane.EnrollRequest{
			Grant: "tsg1.b3BhcXVlLWdyYW50LXBheWxvYWQ.c2lnbmF0dXJl", InstallID: fixtureInstallID,
			DevicePublicKey: fixtureDeviceKey, AgeRecipient: fixtureRecipient,
			Hostname: "managed-host", Platform: "linux/amd64",
		}},
		{"enroll response", "enroll-response.schema.json", controlplane.EnrollResponse{
			Organization: "acme",
		}},
		{"config request", "config-request.schema.json", controlplane.ConfigRequest{
			AgentVersion: "0.0.0-test", ConfigVersions: []int{1},
		}},
		{"config response", "config-response.schema.json", controlplane.ConfigResponse{
			Config:    []byte("config_version: 1\n"),
			ExpiresAt: time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
		}},
		// The request's object is a oneOf, so "fully populated" is two instances rather than
		// one: every mirror metadata name, and the heartbeat's lone kind.
		{"v2 authorize request, mirror", "v2/uploads-authorize-request.schema.json", controlplane.AuthorizeRequest{
			WriterID: fixtureWriterID,
			IssuedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
			Objects: []controlplane.UploadObject{{
				ObjectID: "trajectory-1", Key: fixtureMirrorKey, Size: 481239,
				SourceHash: fixtureSourceHash,
				Metadata: controlplane.UploadMetadata{
					ManifestVersion: "1", SourceID: "claude-code-transcripts",
					ShippedHash: fixtureShippedHash, ArtifactClass: "trajectory",
					AgentVersion: "0.0.0-test", ShapeSniff: "ok",
					Derived: "true", EnrichStatus: "ok",
				},
			}},
		}},
		{"v2 authorize request, heartbeat", "v2/uploads-authorize-request.schema.json", controlplane.AuthorizeRequest{
			WriterID: fixtureWriterID,
			IssuedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
			Objects: []controlplane.UploadObject{{
				ObjectID: "heartbeat", Key: fixtureHeartbeatKey, Size: 8261,
				SourceHash: fixtureSourceHash,
				Metadata:   controlplane.UploadMetadata{Kind: "heartbeat"},
			}},
		}},
		{"v2 authorize response", "v2/uploads-authorize-response.schema.json", controlplane.AuthorizeResponse{
			Tickets: []controlplane.Ticket{{
				TicketID: fixtureTicketID, ObjectID: "trajectory-1", Method: "PUT",
				URL:       "https://archive.example.invalid/object?X-Amz-Signature=FIXTURE",
				ExpiresAt: time.Date(2026, 8, 19, 12, 5, 0, 0, time.UTC),
				RequiredHeaders: controlplane.TicketHeaders{
					"x-amz-meta-source-hash":      fixtureSourceHash,
					"x-amz-meta-ticket-id":        fixtureTicketID,
					"x-amz-meta-manifest-version": "1",
					"x-amz-meta-source-id":        "claude-code-transcripts",
					"x-amz-meta-shipped-hash":     fixtureShippedHash,
					"x-amz-meta-artifact-class":   "trajectory",
					"x-amz-meta-agent-version":    "0.0.0-test",
					"x-amz-meta-shape-sniff":      "ok",
					"x-amz-meta-derived":          "true",
					"x-amz-meta-enrich-status":    "ok",
					"x-amz-meta-kind":             "heartbeat",
					"x-amz-tagging":               "class=trajectory",
				},
				ContentLength: 481239, ContentLengthSigned: true,
			}},
		}},
		{"v2 authorize response, already present", "v2/uploads-authorize-response.schema.json", controlplane.AuthorizeResponse{
			Tickets: []controlplane.Ticket{{
				TicketID: fixtureTicketID, ObjectID: "trajectory-1", AlreadyPresent: true,
			}},
		}},
		{"v2 authorize response, gcs", "v2/uploads-authorize-response.schema.json", controlplane.AuthorizeResponse{
			Tickets: []controlplane.Ticket{{
				TicketID: fixtureTicketID, ObjectID: "trajectory-1", Method: "PUT",
				URL:       "https://storage.googleapis.com/archive/object?X-Goog-Signature=FIXTURE",
				ExpiresAt: time.Date(2026, 8, 19, 12, 5, 0, 0, time.UTC),
				RequiredHeaders: controlplane.TicketHeaders{
					"x-goog-meta-source-hash": fixtureSourceHash,
					"x-goog-meta-ticket-id":   fixtureTicketID,
				},
				ContentLength: 481239, ContentLengthSigned: true,
			}},
		}},
		{"v2 authorize response, azure", "v2/uploads-authorize-response.schema.json", controlplane.AuthorizeResponse{
			Tickets: []controlplane.Ticket{{
				TicketID: fixtureTicketID, ObjectID: "trajectory-1", Method: "PUT",
				URL:       "https://archive.blob.core.windows.net/container/object?sig=FIXTURE",
				ExpiresAt: time.Date(2026, 8, 19, 12, 5, 0, 0, time.UTC),
				RequiredHeaders: controlplane.TicketHeaders{
					"x-ms-meta-source_hash": fixtureSourceHash,
					"x-ms-meta-ticket_id":   fixtureTicketID,
					"x-ms-blob-type":        "BlockBlob",
					"x-ms-tags":             "class=trajectory",
				},
				ContentLength: 481239, ContentLengthSigned: true,
			}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := compileWireSchema(t, tc.schema).Validate(doc); err != nil {
				t.Errorf("marshaled struct does not satisfy its schema:\n%s\n%v", payload, err)
			}
		})
	}
}

// fixtureSchema maps a fixture path to the schema that owns it. The auth directory is not
// a message and has no schema.
func fixtureSchema(path string) string {
	dir := pathpkg.Base(pathpkg.Dir(path))
	if dir == "auth" {
		return ""
	}
	kind := "response"
	if strings.Contains(pathpkg.Base(path), "request") {
		kind = "request"
	}
	switch dir {
	case "enroll":
		return "enroll-" + kind + ".schema.json"
	case "config":
		return "config-" + kind + ".schema.json"
	case "uploads-authorize":
		return "v2/uploads-authorize-" + kind + ".schema.json"
	}
	return ""
}

func wireFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := fs.Glob(protocol.FS, "fixtures/*/*/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no embedded protocol fixtures (err: %v)", err)
	}
	return paths
}

// Every good fixture validates; every bad-* fixture is rejected. This is what proves the
// schemas non-vacuous.
func TestFixturesValidate(t *testing.T) {
	for _, path := range wireFixtures(t) {
		schema := fixtureSchema(path)
		if schema == "" {
			continue
		}
		name := pathpkg.Base(pathpkg.Dir(path)) + "/" + pathpkg.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := fs.ReadFile(protocol.FS, path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			err = compileWireSchema(t, schema).Validate(doc)
			if bad := strings.HasPrefix(pathpkg.Base(path), "bad-"); bad && err == nil {
				t.Errorf("%s must be rejected by %s", name, schema)
			} else if !bad && err != nil {
				t.Errorf("%s must validate against %s: %v", name, schema, err)
			}
		})
	}
}

// Fixture, struct, fixture: every good fixture decodes with DisallowUnknownFields and re-marshals
// to the same JSON value, so a spec field the struct lacks or a tag typo fails here.
func TestFixturesRoundTripStructs(t *testing.T) {
	targets := map[string]func() any{
		"enroll-request.schema.json":  func() any { return &controlplane.EnrollRequest{} },
		"enroll-response.schema.json": func() any { return &controlplane.EnrollResponse{} },
		"config-request.schema.json":  func() any { return &controlplane.ConfigRequest{} },
		"config-response.schema.json": func() any { return &controlplane.ConfigResponse{} },

		"v2/uploads-authorize-request.schema.json":  func() any { return &controlplane.AuthorizeRequest{} },
		"v2/uploads-authorize-response.schema.json": func() any { return &controlplane.AuthorizeResponse{} },
	}
	for _, path := range wireFixtures(t) {
		schema := fixtureSchema(path)
		if schema == "" || strings.HasPrefix(pathpkg.Base(path), "bad-") {
			continue
		}
		name := pathpkg.Base(pathpkg.Dir(path)) + "/" + pathpkg.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := fs.ReadFile(protocol.FS, path)
			if err != nil {
				t.Fatal(err)
			}
			v := targets[schema]()
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("fixture does not decode into %T: %v", v, err)
			}
			remarshaled, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(remarshaled, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip through %T changed the document:\nfixture: %s\nrewrote: %s", v, raw, remarshaled)
			}
		})
	}
}

// authFixture covers both versions of fixtures/*/auth/headers.json. Absent members stay zero:
// v1 has no signing prefix and v2 has no config exchange.
type authFixture struct {
	Comment          string         `json:"comment"`
	Organization     string         `json:"organization"`
	InstallID        string         `json:"install_id"`
	DeviceKeySeedHex string         `json:"device_key_seed_hex"`
	DevicePublicKey  string         `json:"device_public_key"`
	ServerTime       time.Time      `json:"server_time"`
	SigningPrefix    string         `json:"signing_prefix"`
	Config           authExchange   `json:"config"`
	Authorize        authExchange   `json:"authorize"`
	Rejected         []authExchange `json:"rejected"`
}

type authExchange struct {
	Name          string `json:"name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Body          string `json:"body"`
	Authorization string `json:"authorization"`
}

func loadAuthFixture(t *testing.T, version string) (authFixture, ed25519.PrivateKey) {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, pathpkg.Join("fixtures", version, "auth", "headers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx authFixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(fx.DeviceKeySeedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("device_key_seed_hex is not a %d-byte hex seed: %v", ed25519.SeedSize, err)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if got := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)); got != fx.DevicePublicKey {
		t.Fatalf("seed derives public key %s, fixture says %s", got, fx.DevicePublicKey)
	}
	return fx, key
}

// The golden authorization headers are internally consistent: each signature verifies over
// the exact body bytes against the fixture's public key.
func TestAuthFixtureGoldensVerify(t *testing.T) {
	fx, key := loadAuthFixture(t, "v1")
	pub := key.Public().(ed25519.PublicKey)
	for name, ex := range map[string]authExchange{"config": fx.Config} {
		want := "Shipper-Device org=" + fx.Organization + ", install=" + fx.InstallID + ", sig="
		if !strings.HasPrefix(ex.Authorization, want) {
			t.Errorf("%s authorization %q does not open with %q", name, ex.Authorization, want)
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ex.Authorization, want))
		if err != nil {
			t.Errorf("%s sig is not base64: %v", name, err)
			continue
		}
		if !ed25519.Verify(pub, []byte(ex.Body), sig) {
			t.Errorf("%s golden signature does not verify over its body", name)
		}
	}
}

// --- v2 upload authorization ---------------------------------------------------------

// v2SigningPrefix is the domain-separating preamble from PROTOCOL.md, spelled out here so a
// silent change to either the fixture or the client fails rather than redefines it.
const v2SigningPrefix = "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"

// v2SigningInput is the signed byte sequence for one method and path: the domain prefix built
// from those fixed protocol values, then the exact body bytes.
func v2SigningInput(method, path, body string) []byte {
	return []byte("trajectory-shipper-upload-authorize-v2\n" + method + "\n" + path + "\n" + body)
}

// v2Fresh mirrors the server's freshness window: 5 minutes old, 1 minute in the future.
func v2Fresh(issuedAt, serverTime time.Time) bool {
	return !issuedAt.Before(serverTime.Add(-5*time.Minute)) && !issuedAt.After(serverTime.Add(time.Minute))
}

func v2Signature(t *testing.T, authorization, organization, installID string) []byte {
	t.Helper()
	want := "Shipper-Device org=" + organization + ", install=" + installID + ", sig="
	if !strings.HasPrefix(authorization, want) {
		t.Fatalf("authorization %q does not open with %q", authorization, want)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, want))
	if err != nil {
		t.Fatalf("sig is not base64: %v", err)
	}
	return sig
}

func authorizationOrganization(authorization string) string {
	const prefix = "Shipper-Device "
	if !strings.HasPrefix(authorization, prefix) {
		return ""
	}
	for _, field := range strings.Split(strings.TrimPrefix(authorization, prefix), ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok && key == "org" {
			return value
		}
	}
	return ""
}

// The v2 golden is internally consistent, and every must-reject case fails for a reason the
// protocol gives: a signature that does not verify, or an issued_at outside the freshness window.
func TestAuthV2FixtureGoldensVerify(t *testing.T) {
	fx, key := loadAuthFixture(t, "v2")
	pub := key.Public().(ed25519.PublicKey)

	if fx.SigningPrefix != v2SigningPrefix {
		t.Fatalf("fixture signing_prefix is %q, protocol says %q", fx.SigningPrefix, v2SigningPrefix)
	}
	if fx.ServerTime.IsZero() {
		t.Fatal("v2 auth fixture carries no server_time, so freshness cannot be judged")
	}

	golden := v2Signature(t, fx.Authorize.Authorization, fx.Organization, fx.InstallID)
	if !ed25519.Verify(pub, v2SigningInput(fx.Authorize.Method, fx.Authorize.Path, fx.Authorize.Body), golden) {
		t.Error("the golden authorize signature does not verify over its domain-separated input")
	}

	for _, rej := range fx.Rejected {
		t.Run(rej.Name, func(t *testing.T) {
			if authorizationOrganization(rej.Authorization) != fx.Organization {
				return
			}
			sig := v2Signature(t, rej.Authorization, fx.Organization, fx.InstallID)
			if !ed25519.Verify(pub, v2SigningInput(rej.Method, rej.Path, rej.Body), sig) {
				return // refused on the signature, which is the point of the case
			}
			var carried struct {
				IssuedAt time.Time `json:"issued_at"`
			}
			if err := json.Unmarshal([]byte(rej.Body), &carried); err != nil {
				t.Fatalf("rejected body does not parse: %v", err)
			}
			if v2Fresh(carried.IssuedAt, fx.ServerTime) {
				t.Errorf("this case verifies AND is fresh at %s, so nothing refuses it",
					fx.ServerTime.Format(time.RFC3339))
			}
		})
	}
}

// The v1 and v2 signing inputs differ over the same bytes: the fixture's "legacy body-only
// signature" is exactly what v1 signing produces, and it is not the v2 golden.
func TestV2SigningInputIsDomainSeparated(t *testing.T) {
	fx, key := loadAuthFixture(t, "v2")

	var legacy authExchange
	for _, rej := range fx.Rejected {
		if rej.Name == "legacy body-only signature" {
			legacy = rej
		}
	}
	if legacy.Body == "" {
		t.Fatal("v2 auth fixture has no 'legacy body-only signature' case to compare against")
	}

	bodyOnly := base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(legacy.Body)))
	domainSeparated := base64.StdEncoding.EncodeToString(
		ed25519.Sign(key, v2SigningInput(fx.Authorize.Method, fx.Authorize.Path, fx.Authorize.Body)))

	if got := base64.StdEncoding.EncodeToString(v2Signature(t, legacy.Authorization, fx.Organization, fx.InstallID)); got != bodyOnly {
		t.Errorf("v1 body-only signing produces %s, the fixture's legacy case is %s", bodyOnly, got)
	}
	if got := base64.StdEncoding.EncodeToString(v2Signature(t, fx.Authorize.Authorization, fx.Organization, fx.InstallID)); got != domainSeparated {
		t.Errorf("v2 signing produces %s, the golden is %s", domainSeparated, got)
	}
	if bodyOnly == domainSeparated {
		t.Error("v1 and v2 signatures agree over the same body: the domain separation is not there")
	}
}

// The client reproduces the golden authorize exchange byte for byte: same body, same
// Authorization header over the domain-separated input.
func TestClientReproducesGoldenAuthorizeHeader(t *testing.T) {
	fx, key := loadAuthFixture(t, "v2")
	requestFixture, err := fs.ReadFile(protocol.FS, "fixtures/v2/uploads-authorize/request.json")
	if err != nil {
		t.Fatal(err)
	}
	responseFixture, err := fs.ReadFile(protocol.FS, "fixtures/v2/uploads-authorize/response.json")
	if err != nil {
		t.Fatal(err)
	}
	var req controlplane.AuthorizeRequest
	dec := json.NewDecoder(bytes.NewReader(requestFixture))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("request fixture does not decode into AuthorizeRequest: %v", err)
	}

	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAuth, gotBody, gotPath = r.Header.Get("Authorization"), string(body), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write(responseFixture)
	}))
	defer srv.Close()

	client, err := controlplane.New(controlplane.Options{
		Endpoint:     srv.URL,
		InstallID:    fx.InstallID,
		Organization: fx.Organization,
		DeviceKey:    key,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.AuthorizeUploads(context.Background(), req)
	if err != nil {
		t.Fatalf("authorize against the fixture response: %v", err)
	}

	if gotPath != fx.Authorize.Path {
		t.Errorf("client posted to %s, protocol path is %s", gotPath, fx.Authorize.Path)
	}
	if gotBody != fx.Authorize.Body {
		t.Errorf("client posted body\n  %s\ngolden is\n  %s", gotBody, fx.Authorize.Body)
	}
	if gotAuth != fx.Authorize.Authorization {
		t.Errorf("client built header\n  %s\ngolden is\n  %s", gotAuth, fx.Authorize.Authorization)
	}

	var want controlplane.AuthorizeResponse
	if err := json.Unmarshal(responseFixture, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("decoded response %+v does not match the fixture %+v", resp, want)
	}
}
