package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

func (p *Accounts) collectCursor(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	values, token, present, err := cursorAccount(ctx, req)
	if !present {
		return nil, false
	}
	body, _ := json.Marshal(values)
	out = append(out, localAccount(req, "cursor.local.account", body, err))
	for _, endpoint := range []string{"GetPlanInfo", "GetCurrentPeriodUsage"} {
		out = append(out, p.cursorCall(ctx, token, endpoint, "{}", accountObservation{Source: "cursor.dashboard." + endpoint, ObservedAt: req.Now().UTC()}))
	}
	return out, true
}

func cursorAccount(ctx context.Context, req Request) (map[string]json.RawMessage, string, bool, error) {
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return nil, "", false, nil
	}
	values, token, err := sqliteread.CursorAccount(ctx, path)
	return values, token, true, err
}

// cursorCall asks one DashboardService endpoint; as with fetch, obs names the call and receives the answer.
func (p *Accounts) cursorCall(ctx context.Context, token, endpoint, body string, obs accountObservation) accountObservation {
	if token == "" {
		obs.Error = "credentials_unavailable"
		return obs
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/"+endpoint, strings.NewReader(body))
	if err != nil {
		obs.Error = "request_failed"
		return obs
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "quesma-shipper")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	return p.fetch(obs, request)
}
