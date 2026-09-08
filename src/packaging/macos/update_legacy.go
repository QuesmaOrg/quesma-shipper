//go:build darwin

package macos

import (
	"bytes"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	selfapply "github.com/creativeprojects/go-selfupdate/update"
)

func legacyUpdateTarget(release common.Release) string {
	return release.Targets[runtime.GOOS+"/"+runtime.GOARCH]
}

func applyLegacyTarget(raw []byte) error {
	return selfapply.Apply(bytes.NewReader(raw), selfapply.Options{})
}
