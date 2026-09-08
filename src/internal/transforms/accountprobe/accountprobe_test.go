package accountprobe

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
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

// A JWT whose claims carry the plan, signed by nobody: the probe reads claims, not signatures.
func codexJWT(t *testing.T, claims string) string {
	t.Helper()
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"RS256"}`) + "." + seg(claims) + "." + seg("sig")
}

func TestCodexExtractsPlanFromTokenClaimsWithoutTheToken(t *testing.T) {
	tok := codexJWT(t, `{"email": "dev@example.com",
	  "https://api.openai.com/auth": {"chatgpt_plan_type": "pro",
	    "chatgpt_subscription_active_until": "2026-09-03T09:00:24+00:00",
	    "chatgpt_user_id": "user-SHOULD-NOT-SHIP"}}`)
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(`{"auth_mode": "chatgpt",
	  "OPENAI_API_KEY": "`+codexToken+`",
	  "tokens": {"id_token": "`+tok+`", "access_token": "`+codexToken+`",
	    "refresh_token": "`+codexToken+`", "account_id": "acc-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := NewCodex().Enrich(transforms.Input{DBPath: p})
	if len(res.Objects) != 1 || res.Errors != 0 {
		t.Fatalf("want 1 object, got %+v", res)
	}
	out := string(res.Objects[0].Payload)
	for _, want := range []string{`"email":"dev@example.com"`, `"plan":"pro"`,
		`"auth_mode":"chatgpt"`, `"active_until":"2026-09-03T09:00:24+00:00"`} {
		if !strings.Contains(out, want) {
			t.Errorf("payload missing %s: %s", want, out)
		}
	}
	for _, banned := range []string{codexToken, tok, "SHOULD-NOT-SHIP", "acc-1"} {
		if strings.Contains(out, banned) {
			t.Errorf("payload leaked %q: %s", banned, out)
		}
	}
}

func TestCodexMangledTokenIsAnErrorNotALeak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(`{"tokens": {"id_token": "not.a.jwt.at.all"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := NewCodex().Enrich(transforms.Input{DBPath: p})
	if res.Errors == 0 || len(res.Objects) != 0 {
		t.Fatalf("mangled token must fail closed: %+v", res)
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
