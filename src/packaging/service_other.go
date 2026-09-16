//go:build !darwin && !linux && !windows

package packaging

import (
	"context"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func UninstallService() (ServiceKind, error) { return serviceUnsupported, nil }
func serviceState(context.Context) ServiceStatus {
	return ServiceStatus{Kind: serviceUnsupported, Detail: "no supervision mechanism on " + runtime.GOOS}
}
func RestartService(context.Context) error            { return nil }
func RestartCommand() string                          { return "" }
func RemoveProgram(executable string) (string, error) { return common.RemoveProgram(executable) }
func SameProgram(a, b string) bool                    { return a == b }
func ProgramRemovalDeferred() bool                    { return false }
func RemovalUnverified(error) bool                    { return false }
