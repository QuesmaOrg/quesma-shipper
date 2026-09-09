package packaging

import (
	"context"
	"io"

	"github.com/QuesmaOrg/quesma-shipper/packaging/macos"
)

// MigrateLegacyInstall is rename-bridge glue; see packaging/macos/legacy.go. Delete with it.
func MigrateLegacyInstall(ctx context.Context, out io.Writer) (bool, error) {
	return macos.MigrateLegacyInstall(ctx, out)
}
