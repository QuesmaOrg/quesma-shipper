//go:build !darwin

package packaging

import "github.com/QuesmaOrg/quesma-shipper/packaging/common"

func updateTarget(release common.Release) string { return common.BinaryTarget(release) }
func applyTarget(raw []byte, _ string) error     { return common.ApplyBinary(raw) }
