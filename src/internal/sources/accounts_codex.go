package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
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
	parts := strings.Split(tokens.ID, ".")
	if len(parts) == 3 {
		claims, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
		if decodeErr == nil && json.Valid(claims) {
			metadata["id_token_claims"] = claims
		}
	}
	body, _ := json.Marshal(metadata)
	out = append(out, localAccount(req, "codex.local.account", body, err))
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
