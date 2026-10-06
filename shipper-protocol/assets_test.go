package protocol_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
	"testing"
	"time"

	protocol "github.com/QuesmaOrg/shipper-protocol"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compileSchema(t *testing.T, assetPath string) *jsonschema.Schema {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, assetPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s is not JSON: %v", assetPath, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(assetPath, doc); err != nil {
		t.Fatalf("add %s: %v", assetPath, err)
	}
	schema, err := compiler.Compile(assetPath)
	if err != nil {
		t.Fatalf("compile %s: %v", assetPath, err)
	}
	return schema
}

type authCase struct {
	Name          string `json:"name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Body          string `json:"body"`
	Authorization string `json:"authorization"`
}

type authFixture struct {
	InstallID        string     `json:"install_id"`
	Organization     string     `json:"organization"`
	DeviceKeySeedHex string     `json:"device_key_seed_hex"`
	DevicePublicKey  string     `json:"device_public_key"`
	ServerTime       string     `json:"server_time"`
	SigningPrefix    string     `json:"signing_prefix"`
	Config           authCase   `json:"config"`
	Authorize        authCase   `json:"authorize"`
	Rejected         []authCase `json:"rejected"`
}

func loadAuthFixture(t *testing.T, assetPath string) (authFixture, ed25519.PublicKey) {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, assetPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture authFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(fixture.DeviceKeySeedHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if got := base64.StdEncoding.EncodeToString(publicKey); got != fixture.DevicePublicKey {
		t.Fatalf("derived public key %q, want %q", got, fixture.DevicePublicKey)
	}
	return fixture, publicKey
}

func locatorMatches(header string, fixture authFixture) bool {
	if !strings.HasPrefix(header, "Shipper-Device ") {
		return false
	}
	fields := strings.Split(strings.TrimPrefix(header, "Shipper-Device "), ", ")
	values := map[string]string{}
	for _, field := range fields {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return false
		}
		values[name] = value
	}
	return values["org"] == fixture.Organization && values["install"] == fixture.InstallID
}

func signatureFromHeader(t *testing.T, header string) []byte {
	t.Helper()
	_, encoded, ok := strings.Cut(header, "sig=")
	if !ok {
		return nil
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	return signature
}

func signingInput(tc authCase) []byte {
	prefix := "trajectory-shipper-upload-authorize-v2\n" + tc.Method + "\n" + tc.Path + "\n"
	return append([]byte(prefix), []byte(tc.Body)...)
}

func TestV1ConfigAuthorizationSignatureFixture(t *testing.T) {
	fixture, publicKey := loadAuthFixture(t, "fixtures/v1/auth/headers.json")
	if !locatorMatches(fixture.Config.Authorization, fixture) {
		t.Fatal("golden v1 authorization locator does not match its install and organization")
	}
	if !ed25519.Verify(publicKey, []byte(fixture.Config.Body), signatureFromHeader(t, fixture.Config.Authorization)) {
		t.Fatal("golden v1 authorization signature does not verify")
	}
	for _, tc := range fixture.Rejected {
		t.Run(tc.Name, func(t *testing.T) {
			if ed25519.Verify(publicKey, []byte(tc.Body), signatureFromHeader(t, tc.Authorization)) && locatorMatches(tc.Authorization, fixture) {
				t.Fatal("must-reject authorization passed signature and locator checks")
			}
		})
	}
}

func TestV2UploadAuthorizationSignatureFixture(t *testing.T) {
	fixture, publicKey := loadAuthFixture(t, "fixtures/v2/auth/headers.json")
	if want := "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"; fixture.SigningPrefix != want {
		t.Fatalf("signing prefix %q, want %q", fixture.SigningPrefix, want)
	}
	if !locatorMatches(fixture.Authorize.Authorization, fixture) {
		t.Fatal("golden v2 authorization locator does not match its install and organization")
	}
	if !ed25519.Verify(publicKey, signingInput(fixture.Authorize), signatureFromHeader(t, fixture.Authorize.Authorization)) {
		t.Fatal("golden v2 authorization signature does not verify")
	}
	serverTime, err := time.Parse(time.RFC3339, fixture.ServerTime)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Rejected {
		t.Run(tc.Name, func(t *testing.T) {
			signatureOK := ed25519.Verify(publicKey, signingInput(tc), signatureFromHeader(t, tc.Authorization))
			var envelope struct {
				IssuedAt string `json:"issued_at"`
			}
			if err := json.Unmarshal([]byte(tc.Body), &envelope); err != nil {
				t.Fatal(err)
			}
			issuedAt, timeErr := time.Parse(time.RFC3339, envelope.IssuedAt)
			fresh := timeErr == nil && !issuedAt.Before(serverTime.Add(-5*time.Minute)) && !issuedAt.After(serverTime.Add(time.Minute))
			if signatureOK && fresh && locatorMatches(tc.Authorization, fixture) {
				t.Fatal("must-reject authorization passed signature, freshness and locator checks")
			}
		})
	}
}

func TestSchemaFixtures(t *testing.T) {
	tests := []struct {
		name           string
		fixturePattern string
		requestSchema  string
		responseSchema string
	}{
		{"v1 enroll", "fixtures/v1/enroll/*.json", "schemas/enroll-request.schema.json", "schemas/enroll-response.schema.json"},
		{"v1 config", "fixtures/v1/config/*.json", "schemas/config-request.schema.json", "schemas/config-response.schema.json"},
		{"v2 upload authorization", "fixtures/v2/uploads-authorize/*.json", "schemas/v2/uploads-authorize-request.schema.json", "schemas/v2/uploads-authorize-response.schema.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := compileSchema(t, tc.requestSchema)
			response := compileSchema(t, tc.responseSchema)
			paths, err := fs.Glob(protocol.FS, tc.fixturePattern)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) == 0 {
				t.Fatalf("no fixtures match %s", tc.fixturePattern)
			}
			for _, assetPath := range paths {
				t.Run(path.Base(assetPath), func(t *testing.T) {
					raw, err := fs.ReadFile(protocol.FS, assetPath)
					if err != nil {
						t.Fatal(err)
					}
					doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
					if err != nil {
						t.Fatalf("fixture is not JSON: %v", err)
					}
					schema := request
					if strings.Contains(path.Base(assetPath), "response") {
						schema = response
					}
					err = schema.Validate(doc)
					bad := strings.HasPrefix(path.Base(assetPath), "bad-")
					if bad && err == nil {
						t.Fatal("bad fixture unexpectedly satisfies its schema")
					}
					if !bad && err != nil {
						t.Fatalf("golden fixture does not satisfy its schema: %v", err)
					}
				})
			}
		})
	}
}
