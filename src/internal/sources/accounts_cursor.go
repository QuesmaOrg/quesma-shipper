package sources

import (
	"context"
	"encoding/json"
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
		out = append(out, p.fetchWithToken(ctx, req, "cursor.dashboard."+endpoint, token,
			"POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/"+endpoint, strings.NewReader("{}"),
			map[string]string{"Content-Type": "application/json", "Connect-Protocol-Version": "1"}))
	}
	return out, true
}
