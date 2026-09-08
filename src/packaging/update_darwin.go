package packaging

import (
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	"github.com/QuesmaOrg/quesma-shipper/packaging/macos"
)

func updateTarget(release common.Release) string   { return macos.UpdateTarget(release) }
func applyTarget(raw []byte, version string) error { return macos.ApplyTarget(raw, version) }
