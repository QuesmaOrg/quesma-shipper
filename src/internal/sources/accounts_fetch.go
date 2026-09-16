package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

const accountResponseLimit = 1 << 20

func (p *Accounts) collect(ctx context.Context, req Request) ([]accountObservation, bool) {
	env := req.Env
	if env.Lookup == nil {
		env.Lookup = func(string) (string, bool) { return "", false }
	}
	var out []accountObservation
	local := func(source string, body json.RawMessage, err error) {
		obs := accountObservation{Source: source, ObservedAt: req.Now().UTC(), Body: body}
		if len(body) > accountResponseLimit {
			obs.Error = "response_too_large"
			obs.Body = nil
		}
		if err != nil {
			obs.Error = "store_unreadable"
			obs.Body = nil
		}
		out = append(out, obs)
	}
	fetch := func(source, method, endpoint, token, accountID string) {
		obs := accountObservation{Source: source, ObservedAt: req.Now().UTC()}
		if token == "" {
			obs.Error = "credentials_unavailable"
		} else {
			obs = p.fetch(ctx, obs, method, endpoint, token, accountID)
		}
		out = append(out, obs)
	}
	switch req.Source.ID {
	case "claude-account":
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
			local("claude.local.oauthAccount", doc["oauthAccount"], err)
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
		token := creds.OAuth.Token
		scoped := false
		for _, scope := range creds.OAuth.Scopes {
			if scope == "user:profile" {
				scoped = true
			}
		}
		if credentialErr != nil || !scoped {
			token = ""
		}
		fetch("claude.oauth.profile", "GET", "https://api.anthropic.com/api/oauth/profile", token, "")
		fetch("claude.oauth.usage", "GET", "https://api.anthropic.com/api/oauth/usage", token, "")
	case "codex-account":
		home := req.Source.Root
		path := filepath.Join(home, "auth.json")
		if !accountPathExists(home) {
			return nil, false
		}
		doc, err := accountJSON(path, accountResponseLimit)
		var tokens struct {
			Access    string `json:"access_token"`
			ID        string `json:"id_token"`
			AccountID string `json:"account_id"`
		}
		if tokenErr := json.Unmarshal(doc["tokens"], &tokens); tokenErr != nil {
			tokens.Access = ""
		}
		metadata := map[string]json.RawMessage{}
		if mode := doc["auth_mode"]; len(mode) > 0 {
			metadata["auth_mode"] = mode
		}
		parts := strings.Split(tokens.ID, ".")
		if len(parts) == 3 {
			claims, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
			if decodeErr == nil && json.Valid(claims) {
				metadata["id_token_claims"] = claims
			}
		}
		body, _ := json.Marshal(metadata)
		local("codex.local.account", body, err)
		fetch("codex.wham.usage", "GET", "https://chatgpt.com/backend-api/wham/usage", tokens.Access, tokens.AccountID)
	case "cursor-account":
		path := filepath.Join(req.Source.Root, "state.vscdb")
		if !accountPathExists(path) {
			return nil, false
		}
		values, token, err := sqliteread.CursorAccount(ctx, path)
		body, _ := json.Marshal(values)
		local("cursor.local.account", body, err)
		fetch("cursor.dashboard.GetPlanInfo", "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/GetPlanInfo", token, "")
		fetch("cursor.dashboard.GetCurrentPeriodUsage", "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage", token, "")
	default:
		local("account", nil, fmt.Errorf("unsupported account source"))
	}
	return out, true
}

func accountPathExists(path string) bool { _, err := os.Stat(path); return !os.IsNotExist(err) }

func accountJSON(path string, limit int64) (map[string]json.RawMessage, error) {
	raw, _, err := platform.ReadWhole(path, limit)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	err = json.Unmarshal(raw, &doc)
	return doc, err
}

func (p *Accounts) fetch(ctx context.Context, obs accountObservation, method, endpoint, token, accountID string) accountObservation {
	client := http.Client{Timeout: 10 * time.Second}
	if p.client != nil {
		client = *p.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader("{}")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		obs.Error = "request_failed"
		return obs
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "quesma-shipper")
	if request.URL.Host == "api.anthropic.com" {
		request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	if accountID != "" {
		request.Header.Set("ChatGPT-Account-Id", accountID)
	}
	if method == "POST" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Connect-Protocol-Version", "1")
	}
	response, err := client.Do(request)
	if err != nil {
		obs.Error = "request_failed"
		return obs
	}
	defer response.Body.Close()
	obs.HTTPStatus = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		obs.Error = "http_error"
		return obs
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, accountResponseLimit+1))
	if err != nil {
		obs.Error = "response_unreadable"
		return obs
	}
	if len(raw) > accountResponseLimit {
		obs.Error = "response_too_large"
		return obs
	}
	if !json.Valid(raw) {
		obs.Error = "invalid_json"
		return obs
	}
	obs.Body = raw
	return obs
}
