package packaging

import (
	"context"
	"os"

	"github.com/QuesmaOrg/quesma-shipper/packaging/macos"
)

func UninstallService() (ServiceKind, error)          { return serviceLaunchd, macos.UninstallService() }
func serviceState(ctx context.Context) ServiceStatus  { return macos.ServiceState(ctx) }
func RestartService(ctx context.Context) error        { return macos.RestartService(ctx) }
func RestartCommand() string                          { return macos.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return macos.RemoveProgram(executable) }
func SameProgram(a, b string) bool                    { return a == b }
func ProgramRemovalDeferred() bool                    { return false }
func PostInstall() error                              { return macos.PostInstall() }
func RemovalUnverified(error) bool                    { return false }
func ManagedRunLog(stateDir string) (*os.File, error) { return macos.ManagedRunLog(stateDir) }
func ManagedEnrollment() (string, string, error)      { return macos.ManagedEnrollment() }
func ValidateUserUninstall() error                    { return macos.ValidateUserUninstall() }
func ValidateRun() error                              { return macos.ValidateRun() }
func PreUninstallSystem() error                       { return macos.PreUninstallSystem() }
