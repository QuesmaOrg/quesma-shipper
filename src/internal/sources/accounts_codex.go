package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func (p *Accounts) collectCodex(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
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
	if accountValuePresent(doc["OPENAI_API_KEY"]) {
		metadata["OPENAI_API_KEY"] = json.RawMessage("true")
	}
	parts := strings.Split(tokens.ID, ".")
	if len(parts) == 3 {
		claims, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
		if decodeErr == nil && json.Valid(claims) {
			metadata["id_token_claims"] = claims
		}
	}
	body, _ := json.Marshal(metadata)
	out = append(out, localAccount(req, "codex.local.account", body, err))
	config, _, configErr := platform.ReadWhole(filepath.Join(home, "config.toml"), accountResponseLimit)
	if errors.Is(configErr, fs.ErrNotExist) {
		configErr = nil
	}
	stored := map[string]string{}
	if provider, ok := tomlTopLevelString(config, "model_provider"); ok {
		stored["model_provider"] = provider
	}
	body, _ = json.Marshal(stored)
	out = append(out, localAccount(req, "codex.local.config", body, configErr))
	obs := accountObservation{Source: "codex.wham.usage", ObservedAt: req.Now().UTC()}
	if tokens.Access == "" {
		obs.Error = "credentials_unavailable"
	} else {
		request, err := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/backend-api/wham/usage", nil)
		if err != nil {
			obs.Error = "request_failed"
		} else {
			request.Header.Set("Authorization", "Bearer "+tokens.Access)
			request.Header.Set("Accept", "application/json")
			request.Header.Set("User-Agent", "quesma-shipper")
			if tokens.AccountID != "" {
				request.Header.Set("ChatGPT-Account-Id", tokens.AccountID)
			}
			obs = p.fetch(obs, request)
		}
	}
	out = append(out, obs)
	return out, true
}

// tomlTopLevelString reads one quoted string key before the first table, as Codex's top-level settings are.
func tomlTopLevelString(raw []byte, key string) (string, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		name, value, ok := strings.Cut(line, "=")
		value = strings.TrimSpace(value)
		if !ok || strings.TrimSpace(name) != key || len(value) < 2 || value[0] != '"' && value[0] != '\'' {
			continue
		}
		quote := value[:1]
		if strings.HasPrefix(value, strings.Repeat(quote, 3)) {
			quote = strings.Repeat(quote, 3)
		}
		if end := strings.Index(value[len(quote):], quote); end > 0 {
			return value[len(quote) : len(quote)+end], true
		}
	}
	return "", false
}
