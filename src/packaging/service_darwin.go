package packaging

import (
	"context"

	"github.com/QuesmaOrg/quesma-shipper/packaging/macos"
)

func UninstallService() (ServiceKind, error)          { return serviceLaunchd, macos.UninstallService() }
func serviceState(ctx context.Context) ServiceStatus  { return macos.ServiceState(ctx) }
func RestartService(ctx context.Context) error        { return macos.RestartService(ctx) }
func RestartCommand() string                          { return macos.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return macos.RemoveProgram(executable) }
func SameProgram(a, b string) bool                    { return a == b }
func ProgramRemovalDeferred() bool                    { return false }
func PostInstall() (ServiceStatus, error)             { return macos.PostInstall() }
func RemovalUnverified(error) bool                    { return false }
