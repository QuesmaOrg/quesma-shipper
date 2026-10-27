package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

const testAnalyticsKeyARN = "arn:aws:kms:eu-central-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"

func testAnalyticsIdentity(t *testing.T, org string) (*age.X25519Identity, AnalyticsIdentity) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity, AnalyticsIdentity{Organization: org, Recipient: identity.Recipient().String(),
		Ciphertext: base64.StdEncoding.EncodeToString([]byte("opaque KMS ciphertext blob")), KMSKey: testAnalyticsKeyARN}
}

func createWithAnalytics(t *testing.T, server *Server, slug string, analytics *AnalyticsIdentity) int {
	t.Helper()
	body, _ := json.Marshal(adminCreateOrganizationRequest{Slug: slug, DisplayName: slug, AgeRecipients: twoRecipients(t),
		AllowQuesmaETL: ptr(false), AnalyticsIdentity: analytics})
	return serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(body), testAdminCredential)).Code
}

func servedRecipients(t *testing.T, server *Server, slug string) []string {
	t.Helper()
	manager, _ := server.manager.ForOrganization(slug)
	cfg, _, err := manager.LoadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Encryption struct {
			AdditionalRecipients []string `yaml:"additional_recipients"`
		} `yaml:"encryption"`
	}
	if err := yaml.Unmarshal([]byte(renderConfig(server.defaults.resolve(cfg))), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Encryption.AdditionalRecipients
}

// The answer is given once, when the organization is created. A configuration write keeps it and
// cannot name it: replacing it would leave the history sealed to a ciphertext nobody keeps.
func TestAnAnalyticsIdentityIsSetOnceAtCreation(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	_, analytics := testAnalyticsIdentity(t, "acme")
	if code := createWithAnalytics(t, server, "acme", &analytics); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	if code := createWithAnalytics(t, server, "globex", nil); code != http.StatusCreated {
		t.Fatalf("create without = %d", code)
	}

	w := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential))
	var cfg FleetConfig
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || cfg.AnalyticsIdentity == nil || *cfg.AnalyticsIdentity != analytics {
		t.Fatalf("config = %s, %v; want the analytics identity", w.Body, err)
	}

	put := func(body string) *http.Response {
		t.Helper()
		etag := serveAdmin(t, server, adminRequest("GET", "/v1/admin/orgs/acme/config", "", testAdminCredential)).Header().Get("ETag")
		req := adminRequest("PUT", "/v1/admin/orgs/acme/config", body, testAdminCredential)
		req.Header.Set("If-Match", etag)
		return serveAdmin(t, server, req).Result()
	}
	if res := put(configJSON(t, twoRecipients(t))); res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /config = %d", res.StatusCode)
	}
	_, other := testAnalyticsIdentity(t, "acme")
	replace, _ := json.Marshal(map[string]any{"age_recipients": twoRecipients(t), "include_install_recipient": true, "analytics_identity": other})
	if res := put(string(replace)); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT /config naming analytics_identity = %d, want 400", res.StatusCode)
	}

	served := servedRecipients(t, server, "acme")
	if len(served) != 3 || served[2] != analytics.Recipient {
		t.Errorf("acme is served %v, want its two custodians and then the analytics recipient", served)
	}
	if served := servedRecipients(t, server, "globex"); len(served) != 2 {
		t.Errorf("globex, created without an analytics identity, is served %v, want its two custodians only", served)
	}
}

func TestAnAnalyticsIdentityIsCheckedBeforeItIsStored(t *testing.T) {
	_, valid := testAnalyticsIdentity(t, "acme")
	custodian := twoRecipients(t)[0]
	for name, mutate := range map[string]func(*AnalyticsIdentity){
		"the Quesma recipient":          func(a *AnalyticsIdentity) { a.Recipient = quesmaETLAgeRecipient },
		"a private identity":            func(a *AnalyticsIdentity) { a.Recipient = "AGE-SECRET-KEY-1QQQ" },
		"no recipient":                  func(a *AnalyticsIdentity) { a.Recipient = "" },
		"made for another organization": func(a *AnalyticsIdentity) { a.Organization = "acme-prod" },
		"an alias, not a key ARN": func(a *AnalyticsIdentity) {
			a.KMSKey = "arn:aws:kms:eu-central-1:123456789012:alias/fleet-manager-analytics"
		},
		"no ciphertext":            func(a *AnalyticsIdentity) { a.Ciphertext = "" },
		"ciphertext not in base64": func(a *AnalyticsIdentity) { a.Ciphertext = "not base64!" },
		"the identity in base64": func(a *AnalyticsIdentity) {
			a.Ciphertext = base64.StdEncoding.EncodeToString([]byte("AGE-SECRET-KEY-1QQQ\n"))
		},
		"ciphertext over 6 KiB": func(a *AnalyticsIdentity) {
			a.Ciphertext = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 6145))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, store := testMultiTenantAdminServer(t)
			analytics := valid
			mutate(&analytics)
			if code := createWithAnalytics(t, server, "acme", &analytics); code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400", code)
			}
			if _, _, err := store.Get(t.Context(), configKey("acme")); err == nil {
				t.Error("the organization was created anyway")
			}
		})
	}

	t.Run("a custodian's recipient", func(t *testing.T) {
		server, _ := testMultiTenantAdminServer(t)
		analytics := valid
		analytics.Recipient = custodian
		body, _ := json.Marshal(adminCreateOrganizationRequest{Slug: "acme", DisplayName: "acme",
			AgeRecipients: []string{custodian, twoRecipients(t)[0]}, AnalyticsIdentity: &analytics})
		if w := serveAdmin(t, server, adminRequest("POST", "/v1/admin/orgs", string(body), testAdminCredential)); w.Code != http.StatusBadRequest {
			t.Fatalf("create = %d, want 400", w.Code)
		}
	})
}

// What a shipper sealed before any processor was granted access opens with the identity a granted
// processor decrypts later: the recipient is served from the first configuration on.
func TestHistorySealedBeforeAGrantOpensWithTheAnalyticsIdentity(t *testing.T) {
	server, _ := testMultiTenantAdminServer(t)
	identity, analytics := testAnalyticsIdentity(t, "acme")
	if code := createWithAnalytics(t, server, "acme", &analytics); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	parsed, err := age.ParseRecipients(strings.NewReader(strings.Join(servedRecipients(t, server, "acme"), "\n")))
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	writer, err := age.Encrypt(&sealed, parsed...)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("a transcript uploaded on day one"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := age.Decrypt(&sealed, identity)
	if err != nil {
		t.Fatalf("the analytics identity does not open an upload sealed to the served recipients: %v", err)
	}
	if plain, _ := io.ReadAll(reader); string(plain) != "a transcript uploaded on day one" {
		t.Fatalf("opened %q", plain)
	}
}
