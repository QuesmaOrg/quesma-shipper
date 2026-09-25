package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminUIEmbedsCompleteWorkflow(t *testing.T) {
	for _, tc := range []struct {
		path, contentType string
		contains          []string
	}{
		{"/admin/", "text/html; charset=utf-8", []string{"Connect to your fleet", "autocomplete=\"current-password\"", "Organization slug", "org-select", "Add recipient", quesmaETLAgeRecipient, "Quesma's recipient appears here", "age-keygen -o custodian.agekey", "prints its public recipient", "Copy key generation command", "store the private", "in a secure location", "Encrypt uploads with the Quesma ETL public key", "enables Quesma ETL", "analytics", "Quesma cannot access your bucket", "quesma-shipper login --server", "Configuration", "Grants", "Invites", "Installs", "Last vend", "Client version", "Booted", "rename-dialog", "back to showing the install id", "tags.json"}},
		{"/admin/app.js", "text/javascript; charset=utf-8", []string{"sessionStorage", "Authorization", "Bearer ", "If-Match", "getAll('recipients')", "allow_quesma_etl", "/defaults", "deploymentDefaults", "syncQuesmaRecipient", "requestQuesmaDisable", "disable-quesma-dialog", "enroll-command", "/orgs", "selectOrganization", "/grants", "/invites", "/installs", "/installs/seen", "loadSeen", "/installs/tags", "loadTags", "openRename", "navigator.clipboard"}},
		{"/admin/styles.css", "text/css; charset=utf-8", []string{"--vermilion", "Schibsted Grotesk", "field-note", "danger-solid", "dialog", "@media"}},
		{"/admin/date-time.js", "text/javascript; charset=utf-8", []string{"createExpiryPicker", "parseLocalDateTime"}},
		{"/admin/fonts/schibsted-grotesk-latin.woff2", "font/woff2", nil},
		{"/admin/quesma-logo.png", "image/png", nil},
		{"/admin/favicon.svg", "image/svg+xml", nil},
	} {
		t.Run(tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			adminUIHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
			response := recorder.Result()
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			// Exact, not a substring: the type is pinned precisely because leaving it to the
			// host's mime table made it differ between a developer's machine and CI.
			if got := response.Header.Get("Content-Type"); got != tc.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
			for _, want := range tc.contains {
				if !strings.Contains(string(body), want) {
					t.Errorf("body does not contain %q", want)
				}
			}
			if len(response.Cookies()) != 0 {
				t.Fatal("static UI set a cookie")
			}
			if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatal("static UI lacks anti-framing policy")
			}
			// Fonts and the logo are served from the binary, so the policy stays closed to
			// everything the browser could otherwise be talked into fetching elsewhere.
			for _, directive := range []string{"default-src 'none'", "font-src 'self'", "img-src 'self'"} {
				if !strings.Contains(response.Header.Get("Content-Security-Policy"), directive) {
					t.Errorf("Content-Security-Policy lacks %q", directive)
				}
			}
		})
	}
}

func TestAdminUIOmitsOldShipperCommand(t *testing.T) {
	for _, name := range []string{"admin-ui/index.html", "admin-ui/app.js"} {
		raw, err := adminUIAssets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(raw), "quesma-shipper login --server", "")
		if strings.Contains(text, "shipper login --server") {
			t.Errorf("%s still uses the old shipper executable name", name)
		}
	}
}

func TestAdminUIOnlyRemovesQuesmaRecipientThroughCheckbox(t *testing.T) {
	raw, err := adminUIAssets.ReadFile("admin-ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "remove-quesma-recipient") {
		t.Fatal("Quesma recipient has a separate remove control")
	}
}

func TestAdminUIShowsSingleKeygenCommand(t *testing.T) {
	raw, err := adminUIAssets.ReadFile("admin-ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "age-keygen -y") {
		t.Error("index.html redundantly prints the public recipient")
	}
	if strings.Contains(text, ">Copy commands<") {
		t.Error("index.html still has the large text copy button")
	}
	if strings.Count(text, "aria-label=\"Copy key generation command\"") != 2 {
		t.Error("each configuration form must have an accessible copy icon")
	}
}

func TestAdminUIOmitsRemovedDashboardChrome(t *testing.T) {
	raw, err := adminUIAssets.ReadFile("admin-ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, removed := range []string{"Refresh all", "Fleet overview", "class=\"mark\"", "<strong>Fleet Manager</strong>", "autocomplete=\"off\" placeholder=\"fma1"} {
		if strings.Contains(text, removed) {
			t.Errorf("index.html still contains %q", removed)
		}
	}
}

func TestAdminUIOmitsInstallRecipientOption(t *testing.T) {
	html, err := adminUIAssets.ReadFile("admin-ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"name=\"include\"", "Allow each shipper to decrypt its own uploads"} {
		if strings.Contains(string(html), removed) {
			t.Errorf("index.html still contains %q", removed)
		}
	}

	javascript, err := adminUIAssets.ReadFile("admin-ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(javascript), "include_install_recipient: false") {
		t.Error("app.js does not disable per-install recipients")
	}
}

func TestAdminUIHasNoThirdPartyOrPersistentCredentialStorage(t *testing.T) {
	raw, err := adminUIAssets.ReadFile("admin-ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"localStorage", "document.cookie", "http://", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("app.js contains forbidden dependency or storage %q", forbidden)
		}
	}
	for _, required := range []string{"credentials: 'omit'", "addEventListener('close'", "button.disabled = true"} {
		if !strings.Contains(text, required) {
			t.Errorf("app.js lacks browser safety behavior %q", required)
		}
	}
	if strings.Contains(text, "cell(row, '').append") {
		t.Error("app.js prefixes a populated table cell with its empty-value placeholder")
	}
}

func TestServerMountsPublicAdminUI(t *testing.T) {
	server, _ := testAdminServer(t)
	redirect := httptest.NewRecorder()
	server.Handler().ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if redirect.Code != http.StatusMovedPermanently || redirect.Header().Get("Location") != "/admin/" {
		t.Fatalf("admin redirect = %d %q", redirect.Code, redirect.Header().Get("Location"))
	}

	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Connect to your fleet") {
		t.Fatalf("mounted admin UI = %d: %s", page.Code, page.Body.String())
	}
}
