//go:build darwin

package macos

import (
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func UpdateTarget(release common.Release) string {
	if _, ok := currentAppBundle(); ok {
		return release.Targets[macPackageTarget]
	}
	return common.BinaryTarget(release)
}

func ApplyTarget(raw []byte, version string) error {
	if SystemManaged() {
		return common.ErrSystemManaged
	}
	app, ok := currentAppBundle()
	if !ok {
		return common.ApplyBinary(raw)
	}
	return applyAppPackage(raw, app, version)
}
