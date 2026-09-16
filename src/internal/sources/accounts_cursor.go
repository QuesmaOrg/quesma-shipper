package sources

import (
	"context"
	"encoding/json"
	"path/filepath"

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
	out = append(out, p.fetch(ctx, req, "cursor.dashboard.GetPlanInfo", "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/GetPlanInfo", token, ""))
	out = append(out, p.fetch(ctx, req, "cursor.dashboard.GetCurrentPeriodUsage", "POST", "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage", token, ""))
	return out, true
}
