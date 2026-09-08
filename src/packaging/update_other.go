//go:build !darwin

package packaging

import (
	"bytes"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	selfapply "github.com/creativeprojects/go-selfupdate/update"
)

func updateTarget(release common.Release) string {
	return release.Targets[runtime.GOOS+"/"+runtime.GOARCH]
}

func applyTarget(raw []byte, _ string) error {
	return selfapply.Apply(bytes.NewReader(raw), selfapply.Options{})
}
