package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// Every value here that looks like a credential must never appear in what the shipper writes.
var authSecrets = []string{"fixture-access-token", "fixture-refresh-token", "sk-fixture-api-key", "fixture-helper-command", "fixture-env-key"}

func idToken(claims string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

const codexClaims = `{"email":"dev@example.org","https://api.openai.com/auth":{"chatgpt_plan_type":"team",
 "chatgpt_subscription_active_until":"2026-11-01T11:24:39+00:00","chatgpt_user_id":"user-1","chatgpt_account_id":"acct-1",
 "organizations":[{"id":"org-1","title":"Personal","is_default":true}]}}`

func TestAgentAuthPerSignInMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  agentAuth
		want string
	}{
		{"claude subscription from credentials", claudeAuth(claudeAuthInput{
			OAuthAccount: json.RawMessage(`{"accountUuid":"u-1","emailAddress":"dev@example.org","organizationUuid":"o-1","organizationName":"Example","organizationType":"claude_team","billingType":"stripe_subscription"}`),
			Credentials:  []byte(`{"claudeAiOauth":{"accessToken":"fixture-access-token","refreshToken":"fixture-refresh-token","subscriptionType":"team"}}`),
		}), `{"billing":"subscription","auth_method":"oauth","provider":"anthropic","plan":"team","account":{"email":"dev@example.org","user_id":"u-1","org_id":"o-1","org_name":"Example"},"evidence":["credentials:claudeAiOauth.subscriptionType"]}`},
		{"claude subscription when the keychain is locked", claudeAuth(claudeAuthInput{
			OAuthAccount: json.RawMessage(`{"accountUuid":"u-1","organizationType":"claude_max","billingType":"stripe_subscription"}`),
		}), `{"billing":"subscription","auth_method":"oauth","provider":"anthropic","plan":"max","account":{"user_id":"u-1"},"evidence":[".claude.json:oauthAccount.billingType"]}`},
		{"claude key from a helper", claudeAuth(claudeAuthInput{
			OAuthAccount: json.RawMessage(`{"accountUuid":"u-1","billingType":"stripe_subscription"}`),
			Settings:     map[string]json.RawMessage{"apiKeyHelper": json.RawMessage(`"fixture-helper-command"`)},
		}), `{"billing":"api","auth_method":"api_key","provider":"anthropic","account":{"user_id":"u-1"},"evidence":["settings.json:apiKeyHelper"]}`},
		{"claude key in settings env", claudeAuth(claudeAuthInput{
			Settings: map[string]json.RawMessage{"env": json.RawMessage(`{"ANTHROPIC_API_KEY":"fixture-env-key"}`)},
		}), `{"billing":"api","auth_method":"api_key","provider":"anthropic","evidence":["settings.json:env.ANTHROPIC_API_KEY"]}`},
		{"claude with an unset helper", claudeAuth(claudeAuthInput{Settings: map[string]json.RawMessage{"apiKeyHelper": json.RawMessage(`null`)}}),
			`{"billing":"unknown","auth_method":"unknown","evidence":[]}`},
		{"claude key approved at login", claudeAuth(claudeAuthInput{HasAPIKey: true}),
			`{"billing":"api","auth_method":"api_key","provider":"anthropic","evidence":[".claude.json:primaryApiKey"]}`},
		{"claude on bedrock", claudeAuth(claudeAuthInput{
			Credentials: []byte(`{"claudeAiOauth":{"subscriptionType":"max"}}`),
			Settings:    map[string]json.RawMessage{"env": json.RawMessage(`{"CLAUDE_CODE_USE_BEDROCK":"1"}`)},
		}), `{"billing":"cloud","auth_method":"cloud_credentials","provider":"bedrock","evidence":["settings.json:env.CLAUDE_CODE_USE_BEDROCK"]}`},
		{"claude with a key only in the shell", claudeAuth(claudeAuthInput{}),
			`{"billing":"unknown","auth_method":"unknown","evidence":[]}`},
		{"codex subscription", codexAuth(map[string]json.RawMessage{"auth_mode": json.RawMessage(`"chatgpt"`), "OPENAI_API_KEY": json.RawMessage(`null`)}, []byte(codexClaims), nil),
			`{"billing":"subscription","auth_method":"oauth","provider":"openai","plan":"team","subscription_until":"2026-11-01T11:24:39Z","account":{"email":"dev@example.org","user_id":"user-1","org_id":"acct-1","org_name":"Personal"},"evidence":["auth.json:auth_mode"]}`},
		{"codex api key", codexAuth(map[string]json.RawMessage{"auth_mode": json.RawMessage(`"apikey"`), "OPENAI_API_KEY": json.RawMessage(`"sk-fixture-api-key"`)}, nil, nil),
			`{"billing":"api","auth_method":"api_key","provider":"openai","evidence":["auth.json:auth_mode"]}`},
		{"codex api key without a mode", codexAuth(map[string]json.RawMessage{"OPENAI_API_KEY": json.RawMessage(`"sk-fixture-api-key"`)}, nil, nil),
			`{"billing":"api","auth_method":"api_key","provider":"openai","evidence":["auth.json:auth_mode"]}`},
		{"codex on another provider despite a login", codexAuth(map[string]json.RawMessage{"auth_mode": json.RawMessage(`"chatgpt"`)}, []byte(codexClaims),
			[]byte("model = \"some/model\"\nmodel_provider = \"openrouter\" # comment\n\n[model_providers.openrouter]\nenv_key = \"OPENROUTER_API_KEY\"\n")),
			`{"billing":"api","auth_method":"api_key","provider":"openrouter","account":{"email":"dev@example.org","user_id":"user-1","org_id":"acct-1","org_name":"Personal"},"evidence":["config.toml:model_provider"]}`},
		{"codex on a local model", codexAuth(nil, nil, []byte("model_provider = 'ollama'\n")),
			`{"billing":"local","auth_method":"none","provider":"ollama","evidence":["config.toml:model_provider"]}`},
		{"codex provider set only inside a profile", codexAuth(nil, nil, []byte("[profiles.work]\nmodel_provider = \"openrouter\"\n")),
			`{"billing":"unknown","auth_method":"unknown","evidence":[]}`},
		{"cursor plan", cursorAuth(map[string]json.RawMessage{
			"cursorAuth/stripeMembershipType": json.RawMessage(`"enterprise"`), "cursorAuth/cachedEmail": json.RawMessage(`"dev@example.org"`),
			"cursorAuth/cachedTeam": json.RawMessage(`{"teamId": 7, "name": "Example"}`),
		}, true), `{"billing":"subscription","auth_method":"oauth","provider":"cursor","plan":"enterprise","account":{"email":"dev@example.org","org_id":"7","org_name":"Example"},"evidence":["state.vscdb:cursorAuth/stripeMembershipType"]}`},
		{"cursor signed in without a cached plan", cursorAuth(map[string]json.RawMessage{}, true),
			`{"billing":"subscription","auth_method":"oauth","provider":"cursor","evidence":["state.vscdb:cursorAuth/accessToken"]}`},
		{"cursor signed out", cursorAuth(map[string]json.RawMessage{}, false),
			`{"billing":"unknown","auth_method":"unknown","evidence":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.got)
			if err != nil {
				t.Fatal(err)
			}
			if err := formats.ValidateRaw(formats.AgentAuth, body); err != nil {
				t.Fatalf("%v\n%s", err, body)
			}
			var got, want map[string]any
			_ = json.Unmarshal(body, &got)
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			delete(got, "schema")
			delete(got, "agent")
			if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
				t.Fatalf("got  %s\nwant %s", g, w)
			}
			assertNoSecrets(t, body)
		})
	}
}

func TestTomlTopLevelString(t *testing.T) {
	for raw, want := range map[string]string{
		`model_provider = "azure"`:              "azure",
		`model_provider="a#b" # trailing`:       "a#b",
		"# model_provider = \"x\"\n":            "",
		"model_providers = \"x\"\n":             "",
		"[table]\nmodel_provider = \"x\"\n":     "",
		"model_provider = \"x":                  "",
		"model = \"m\"\nmodel_provider = 'y'\n": "y",
	} {
		if got := tomlTopLevelString([]byte(raw), "model_provider"); got != want {
			t.Errorf("%q: got %q want %q", raw, got, want)
		}
	}
}

// The whole snapshot, not only the auth record, keeps every stored credential out.
func TestAccountSnapshotsCarryNoCredentials(t *testing.T) {
	ok := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"plan":"fixture"}`)), Header: http.Header{}}, nil
	}
	t.Run("codex", func(t *testing.T) {
		req := accountFixture(t)
		accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-fixture-api-key",
		 "tokens":{"access_token":"fixture-access-token","refresh_token":"fixture-refresh-token","account_id":"acct-1","id_token":"`+idToken(codexClaims)+`"}}`)
		accountFile(t, filepath.Join(req.Source.Root, "config.toml"), "check_for_update_on_startup = false\n")
		raw := loadAccount(t, req, ok)
		assertAuthRecord(t, raw, "codex.auth", `"billing":"subscription"`)
		if bytes.Contains(raw, []byte(idToken(codexClaims))) {
			t.Fatal("shipped the signed ID token")
		}
	})
	t.Run("claude", func(t *testing.T) {
		req := accountFixture(t)
		req.Source.ID, req.Source.Family = "claude-account", "claude-code"
		home := filepath.Join(req.Env.Home, "claude-home")
		req.Source.Root = home
		req.Env.Lookup = func(k string) (string, bool) { return home, k == "CLAUDE_CONFIG_DIR" }
		accountFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"u-1","billingType":"stripe_subscription"},"primaryApiKey":null}`)
		accountFile(t, filepath.Join(home, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"fixture-access-token","refreshToken":"fixture-refresh-token","scopes":["user:profile"],"subscriptionType":"max"}}`)
		accountFile(t, filepath.Join(home, "settings.json"), `{"env":{"OTHER":"fixture-env-key"}}`)
		raw := loadAccount(t, req, ok)
		assertAuthRecord(t, raw, "claude.auth", `"plan":"max"`)
	})
}

func loadAccount(t *testing.T, req Request, transport accountTransport) []byte {
	t.Helper()
	p := Accounts{client: &http.Client{Transport: transport}, keychain: func(context.Context, string) ([]byte, error) { return nil, errors.New("locked") }}
	d, err := p.Discover(req)
	if err != nil || len(d.Candidates) != 1 {
		t.Fatalf("discover: %+v %v", d, err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, payload.Bytes)
	return payload.Bytes
}

func assertAuthRecord(t *testing.T, raw []byte, source, want string) {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var record accountObservation
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Source != source {
			continue
		}
		if err := formats.ValidateRaw(formats.AgentAuth, record.Body); err != nil || !bytes.Contains(record.Body, []byte(want)) {
			t.Fatalf("%v: %s", err, record.Body)
		}
		return
	}
	t.Fatalf("no %s record: %s", source, raw)
}

func assertNoSecrets(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range authSecrets {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("shipped %q: %s", secret, raw)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
