//go:build darwin

package macos

import (
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func UpdateTarget(release common.Release) string {
	if _, ok := currentAppBundle(); ok {
		return appUpdateTarget(release)
	}
	return rawUpdateTarget(release)
}

func ApplyTarget(raw []byte, version string) error {
	app, ok := currentAppBundle()
	if !ok {
		return applyRawTarget(raw)
	}
	return applyAppPackage(raw, app, version)
}
