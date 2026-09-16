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
	path := filepath.Join(req.Source.Root, "state.vscdb")
	if !accountPathExists(path) {
		return nil, false
	}
	values, token, err := sqliteread.CursorAccount(ctx, path)
	body, _ := json.Marshal(values)
	out = append(out, localAccount(req, "cursor.local.account", body, err))
	for _, endpoint := range []string{"GetPlanInfo", "GetCurrentPeriodUsage"} {
		obs := accountObservation{Source: "cursor.dashboard." + endpoint, ObservedAt: req.Now().UTC()}
		if token == "" {
			obs.Error = "credentials_unavailable"
		} else {
			request, err := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/"+endpoint, strings.NewReader("{}"))
			if err != nil {
				obs.Error = "request_failed"
			} else {
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Accept", "application/json")
				request.Header.Set("User-Agent", "quesma-shipper")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Connect-Protocol-Version", "1")
				obs = p.fetch(obs, request)
			}
		}
		out = append(out, obs)
	}
	return out, true
}
