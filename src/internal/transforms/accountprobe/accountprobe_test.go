package accountprobe

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Fixture tokens; every test asserts they never reach a payload.
const (
	claudeToken = "CLAUDE-OAUTH-ACCESS-TOKEN"
	codexToken  = "CODEX-REFRESH-TOKEN"
)

func claudeStore(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeExtractsPlanTierAndEmailAndNothingElse(t *testing.T) {
	p := claudeStore(t, `{
	  "oauthAccount": {
	    "accountUuid": "84ebfc94-0000-0000-0000-000000000000",
	    "emailAddress": "dev@example.com",
	    "organizationType": "claude_max",
	    "organizationRateLimitTier": "default_claude_max_20x",
	    "userRateLimitTier": null,
	    "seatTier": null,
	    "billingType": "stripe_subscription",
	    "organizationName": "dev's Organization",
	    "organizationRole": "admin"
	  },
	  "projects": {"/home/dev/x": {"history": ["the prompts must never ship"]}},
	  "primaryApiKey": "`+claudeToken+`"
	}`)
	res := NewClaude().Enrich(transforms.Input{DBPath: p})
	if len(res.Objects) != 1 || res.Errors != 0 {
		t.Fatalf("want 1 object, got %+v", res)
	}
	out := string(res.Objects[0].Payload)
	for _, want := range []string{`"email":"dev@example.com"`, `"plan":"claude_max"`,
		`"rate_limit_tier":"default_claude_max_20x"`, `"billing_type":"stripe_subscription"`} {
		if !strings.Contains(out, want) {
			t.Errorf("payload missing %s: %s", want, out)
		}
	}
	for _, banned := range []string{claudeToken, "prompts", "accountUuid", "84ebfc94"} {
		if strings.Contains(out, banned) {
			t.Errorf("payload leaked %q: %s", banned, out)
		}
	}
	if res.Objects[0].NativePath != p+".account.enriched.json" {
		t.Errorf("derived path %q not beside its store", res.Objects[0].NativePath)
	}
}

func TestClaudeUserTierWinsOverOrgTier(t *testing.T) {
	p := claudeStore(t, `{"oauthAccount": {"emailAddress": "dev@example.com",
	  "organizationType": "claude_team", "organizationRateLimitTier": "team_default",
	  "userRateLimitTier": "team_premium_seat"}}`)
	res := NewClaude().Enrich(transforms.Input{DBPath: p})
	if len(res.Objects) != 1 {
		t.Fatalf("want 1 object, got %+v", res)
	}
	if !strings.Contains(string(res.Objects[0].Payload), `"rate_limit_tier":"team_premium_seat"`) {
		t.Errorf("user tier did not win: %s", res.Objects[0].Payload)
	}
}

func TestClaudeMissingStoreAndMissingAccountAreSkipsNotErrors(t *testing.T) {
	res := NewClaude().Enrich(transforms.Input{DBPath: "", Units: make([]transforms.RawUnit, 3)})
	if res.Errors != 0 || res.Skipped != 3 || len(res.Objects) != 0 {
		t.Fatalf("absent store: %+v", res)
	}
	res = NewClaude().Enrich(transforms.Input{DBPath: claudeStore(t, `{"projects": {}}`),
		Units: make([]transforms.RawUnit, 2)})
	if res.Errors != 0 || res.Skipped != 2 || len(res.Objects) != 0 {
		t.Fatalf("logged-out store: %+v", res)
	}
}

func TestDeterminismIsTheChangeSignal(t *testing.T) {
	p := claudeStore(t, `{"oauthAccount": {"emailAddress": "dev@example.com", "organizationType": "claude_max"}}`)
	a := NewClaude().Enrich(transforms.Input{DBPath: p})
	b := NewClaude().Enrich(transforms.Input{DBPath: p})
	if a.Objects[0].OutputHash != b.Objects[0].OutputHash {
		t.Error("same store, different hash: the object would re-ship every flush")
	}
	if !bytes.Equal(a.Objects[0].Payload, b.Objects[0].Payload) {
		t.Error("same store, different bytes")
	}
}

// fakeCodexServer writes a POSIX-shell stand-in for `codex app-server --stdio`. It answers the
// initialize/account-read handshake with the given account line, so no real codex is needed.
func fakeCodexServer(t *testing.T, accountResult string) *Codex {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server uses POSIX shell")
	}
	script := `#!/bin/sh
read line
printf '%s\n' '{"id":1,"result":{"userAgent":"fake","codexHome":"/tmp"}}'
read line
read line
printf '%s\n' '{"method":"remoteControl/status/changed","params":{"status":"disabled"}}'
printf '%s\n' '` + accountResult + `'
`
	path := filepath.Join(t.TempDir(), "codex-fake")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Codex{command: path}
}

func TestCodexReadsPlanFromAppServerWithoutAnyToken(t *testing.T) {
	e := fakeCodexServer(t, `{"id":2,"result":{"account":{"type":"chatgpt","email":"dev@example.com","planType":"team"},"requiresOpenaiAuth":true}}`)
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(`{"tokens":{"id_token":"`+codexToken+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.Enrich(transforms.Input{DBPath: p})
	if len(res.Objects) != 1 || res.Errors != 0 {
		t.Fatalf("want 1 object, got %+v", res)
	}
	out := string(res.Objects[0].Payload)
	for _, want := range []string{`"email":"dev@example.com"`, `"plan":"team"`, `"auth_mode":"chatgpt"`} {
		if !strings.Contains(out, want) {
			t.Errorf("payload missing %s: %s", want, out)
		}
	}
	// The gate file holds a token; the probe must never read or ship it.
	if strings.Contains(out, codexToken) {
		t.Errorf("payload leaked the auth.json token: %s", out)
	}
	if res.Objects[0].NativePath != p+".account.enriched.json" {
		t.Errorf("derived path %q not beside its store", res.Objects[0].NativePath)
	}
}

func TestCodexAppServerErrorIsAnErrorNotASkip(t *testing.T) {
	e := fakeCodexServer(t, `{"id":2,"error":{"code":-32601,"message":"Method not found"}}`)
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.Enrich(transforms.Input{DBPath: p})
	if res.Errors == 0 || len(res.Objects) != 0 {
		t.Fatalf("rpc error must fail loud: %+v", res)
	}
}

func TestCodexSignedOutAppServerIsASkip(t *testing.T) {
	e := fakeCodexServer(t, `{"id":2,"result":{"account":null,"requiresOpenaiAuth":true}}`)
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.Enrich(transforms.Input{DBPath: p, Units: make([]transforms.RawUnit, 1)})
	if res.Errors != 0 || len(res.Objects) != 0 || res.Skipped != 1 {
		t.Fatalf("signed-out app-server: %+v", res)
	}
}

func TestCodexNoAuthJSONIsASkip(t *testing.T) {
	res := NewCodex().Enrich(transforms.Input{DBPath: "", Units: make([]transforms.RawUnit, 2)})
	if res.Errors != 0 || len(res.Objects) != 0 || res.Skipped != 2 {
		t.Fatalf("absent auth.json: %+v", res)
	}
}

func cursorStore(t *testing.T, rows map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestCursorExtractsMembershipAndEmailWhileTokensStayDenied(t *testing.T) {
	const token = "CURSOR-SESSION-TOKEN"
	p := cursorStore(t, map[string]string{
		"cursorAuth/stripeMembershipType": "enterprise",
		"cursorAuth/cachedEmail":          "dev@example.com",
		"cursorAuth/cachedSignUpType":     "Google",
		"cursorAuth/cachedTeam":           `{"teamId": 1, "name": "Quesma"}`,
		"cursorAuth/accessToken":          token,
		"cursorAuth/refreshToken":         token,
	})
	res := NewCursor().Enrich(transforms.Input{DBPath: p, ScratchDir: t.TempDir()})
	if len(res.Objects) != 1 || res.Errors != 0 {
		t.Fatalf("want 1 object, got %+v", res)
	}
	out := string(res.Objects[0].Payload)
	for _, want := range []string{`"plan":"enterprise"`, `"email":"dev@example.com"`,
		`"team":"Quesma"`, `"auth_mode":"Google"`} {
		if !strings.Contains(out, want) {
			t.Errorf("payload missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, token) {
		t.Errorf("payload leaked the session token: %s", out)
	}
}

func TestCursorLoggedOutStoreIsASkip(t *testing.T) {
	p := cursorStore(t, map[string]string{"workbench.something": "x"})
	res := NewCursor().Enrich(transforms.Input{DBPath: p, ScratchDir: t.TempDir(), Units: make([]transforms.RawUnit, 1)})
	if res.Errors != 0 || len(res.Objects) != 0 || res.Skipped != 1 {
		t.Fatalf("logged-out store: %+v", res)
	}
}
