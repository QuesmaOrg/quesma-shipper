//go:build !darwin && !linux && !windows

package app

import (
	"fmt"
	"runtime"
)

func PostInstallPackage() (string, error) {
	return "", fmt.Errorf("postinstall is not supported on %s", runtime.GOOS)
}
