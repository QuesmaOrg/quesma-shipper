//go:build !darwin && !linux

package packaging

import (
	"context"
	"io"
)

// MigrateLegacyInstall is rename-bridge glue with nothing to do here: Windows has no service
// entry naming the binary. Delete with the rest of the bridge.
func MigrateLegacyInstall(context.Context, io.Writer) (bool, error) { return false, nil }
