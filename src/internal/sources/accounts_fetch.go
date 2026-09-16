package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const accountResponseLimit = 1 << 20

func localAccount(req Request, source string, body json.RawMessage, err error) accountObservation {
	obs := accountObservation{Source: source, ObservedAt: req.Now().UTC(), Body: body}
	if len(body) > accountResponseLimit {
		obs.Error = "response_too_large"
		obs.Body = nil
	}
	if err != nil {
		obs.Error = "store_unreadable"
		obs.Body = nil
	}
	return obs
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

func (p *Accounts) fetch(ctx context.Context, req Request, source, method, endpoint, token, accountID string) accountObservation {
	obs := accountObservation{Source: source, ObservedAt: req.Now().UTC()}
	if token == "" {
		obs.Error = "credentials_unavailable"
		return obs
	}
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
