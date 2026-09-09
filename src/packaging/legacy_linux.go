package packaging

import (
	"context"
	"io"

	linuxpkg "github.com/QuesmaOrg/quesma-shipper/packaging/linux"
)

// MigrateLegacyInstall is rename-bridge glue; see packaging/linux/legacy.go. Never an exit: the
// running process already is the new binary. Delete with the rest of the bridge.
func MigrateLegacyInstall(_ context.Context, out io.Writer) (bool, error) {
	return false, linuxpkg.MigrateLegacyInstall(out)
}
