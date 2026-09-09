//go:build !darwin && !linux && !windows

package packaging

import (
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func UninstallService() (ServiceKind, error) { return serviceUnsupported, nil }
func serviceState() ServiceStatus {
	return ServiceStatus{Kind: serviceUnsupported, Detail: "no supervision mechanism on " + runtime.GOOS}
}
func RestartService() error                           { return nil }
func RestartCommand() string                          { return "" }
func RemoveProgram(executable string) (string, error) { return common.RemoveProgram(executable) }
