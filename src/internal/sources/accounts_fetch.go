package sources

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
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

func (p *Accounts) fetch(obs accountObservation, request *http.Request) accountObservation {
	client := http.Client{Timeout: 10 * time.Second}
	if p.client != nil {
		client = *p.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	return withBody(obs, raw)
}

// withBody is the one place a provider response becomes an observation body, so every collector
// shares the size and validity vocabulary.
func withBody(obs accountObservation, raw []byte) accountObservation {
	switch {
	case len(raw) > accountResponseLimit:
		obs.Error = "response_too_large"
	case !json.Valid(raw):
		obs.Error = "invalid_json"
	default:
		obs.Body = raw
	}
	return obs
}
