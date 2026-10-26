package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func (p *Accounts) collectClaude(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	env := req.Env
	if env.Lookup == nil {
		env.Lookup = func(string) (string, bool) { return "", false }
	}
	home := req.Source.Root
	config := filepath.Join(env.Home, ".claude.json")
	service := "Claude Code-credentials"
	override, _ := env.Lookup("CLAUDE_CONFIG_DIR")
	if home != filepath.Join(env.Home, ".claude") || override != "" && filepath.Clean(override) == home {
		config = filepath.Join(home, ".claude.json")
		keychainHome := home
		if override != "" && filepath.Clean(override) == home {
			keychainHome = override
		}
		sum := sha256.Sum256([]byte(keychainHome))
		service = fmt.Sprintf("Claude Code-credentials-%x", sum[:4])
	}
	doc, err := accountJSON(config, 64<<20)
	if err != nil || len(doc["oauthAccount"]) > 0 {
		out = append(out, localAccount(req, "claude.local.oauthAccount", doc["oauthAccount"], err))
	}
	var raw []byte
	if runtime.GOOS == "darwin" && err == nil && bytes.HasPrefix(bytes.TrimSpace(doc["oauthAccount"]), []byte("{")) {
		read := p.keychain
		if read == nil {
			read = readAccountKeychain
		}
		raw, _ = read(ctx, service)
	}
	if len(raw) == 0 {
		raw, _, _ = platform.ReadWhole(filepath.Join(home, ".credentials.json"), accountResponseLimit)
	}
	var creds struct {
		OAuth struct {
			Token  string   `json:"accessToken"`
			Scopes []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	credentialErr := json.Unmarshal(raw, &creds)
	settings, settingsErr := accountJSON(filepath.Join(home, "settings.json"), accountResponseLimit)
	configErr := err
	if errors.Is(configErr, fs.ErrNotExist) || configErr == nil {
		configErr = settingsErr
	}
	if errors.Is(configErr, fs.ErrNotExist) {
		configErr = nil
	}
	body, _ := json.Marshal(claudeConfig(doc, settings, raw))
	out = append(out, localAccount(req, "claude.local.config", body, configErr))
	token := creds.OAuth.Token
	if credentialErr != nil || !slices.Contains(creds.OAuth.Scopes, "user:profile") {
		token = ""
	}
	for _, endpoint := range []string{"profile", "usage"} {
		obs := accountObservation{Source: "claude.oauth." + endpoint, ObservedAt: req.Now().UTC()}
		if token == "" {
			obs.Error = "credentials_unavailable"
		} else {
			request, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/"+endpoint, nil)
			if err != nil {
				obs.Error = "request_failed"
			} else {
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Accept", "application/json")
				request.Header.Set("User-Agent", "quesma-shipper")
				request.Header.Set("anthropic-beta", "oauth-2025-04-20")
				obs = p.fetch(obs, request)
			}
		}
		out = append(out, obs)
	}
	return out, true
}

// Settings that pick who bills Claude Code's model calls. A variable holding a credential ships only as present.
var (
	claudeEnvValues   = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_BASE_URL"}
	claudeEnvPresence = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}
)

// claudeConfig keeps each auth-relevant value under its file and field name, for the ETL to interpret.
func claudeConfig(doc, settings map[string]json.RawMessage, credentials []byte) map[string]any {
	stored := map[string]any{}
	if accountValuePresent(settings["apiKeyHelper"]) {
		stored["apiKeyHelper"] = true
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(settings["env"], &env)
	envOut := map[string]any{}
	for _, k := range claudeEnvValues {
		if accountValuePresent(env[k]) {
			envOut[k] = env[k]
		}
	}
	for _, k := range claudeEnvPresence {
		if accountValuePresent(env[k]) {
			envOut[k] = true
		}
	}
	if len(envOut) > 0 {
		stored["env"] = envOut
	}
	global := map[string]any{}
	if accountValuePresent(doc["primaryApiKey"]) {
		global["primaryApiKey"] = true
	}
	var responses map[string][]json.RawMessage
	if json.Unmarshal(doc["customApiKeyResponses"], &responses) == nil && responses != nil {
		counts := map[string]int{}
		for k, v := range responses {
			counts[k] = len(v)
		}
		global["customApiKeyResponses"] = counts
	}
	var creds struct {
		OAuth map[string]json.RawMessage `json:"claudeAiOauth"`
	}
	_ = json.Unmarshal(credentials, &creds)
	login := map[string]any{}
	for _, k := range []string{"subscriptionType", "rateLimitTier", "scopes"} {
		if accountValuePresent(creds.OAuth[k]) {
			login[k] = creds.OAuth[k]
		}
	}
	return map[string]any{"settings.json": stored, ".claude.json": global, "credentials": map[string]any{"claudeAiOauth": login}}
}

func accountValuePresent(raw json.RawMessage) bool {
	v := string(bytes.TrimSpace(raw))
	return v != "" && v != "null" && v != `""`
}
