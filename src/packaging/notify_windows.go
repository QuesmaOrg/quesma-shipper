package packaging

import (
	"context"
	windowspkg "github.com/QuesmaOrg/quesma-shipper/packaging/windows"
)

func notifyRevocation(ctx context.Context, message string) bool {
	return windowspkg.NotifyRevocation(ctx, message)
}
