package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

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
	now := req.Now().UTC()
	// Without a teamId Cursor returns only the signed-in user's requests, even to a team owner.
	// ponytail: one page of the trailing week; a machine off longer leaves a gap. Add a watermark when it does.
	usage := fmt.Sprintf(`{"startDate":"%d","endDate":"%d","page":1,"pageSize":1000}`, now.Add(-7*24*time.Hour).UnixMilli(), now.UnixMilli())
	for _, call := range []struct{ endpoint, body string }{{"GetPlanInfo", "{}"}, {"GetCurrentPeriodUsage", "{}"}, {"GetFilteredUsageEvents", usage}} {
		obs := accountObservation{Source: "cursor.dashboard." + call.endpoint, ObservedAt: now}
		if token == "" {
			obs.Error = "credentials_unavailable"
		} else {
			request, err := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/"+call.endpoint, strings.NewReader(call.body))
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
