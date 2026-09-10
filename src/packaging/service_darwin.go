package packaging

import "github.com/QuesmaOrg/quesma-shipper/packaging/macos"

func UninstallService() (ServiceKind, error)          { return serviceLaunchd, macos.UninstallService() }
func serviceState() ServiceStatus                     { return macos.ServiceState() }
func RestartService() error                           { return macos.RestartService() }
func RestartCommand() string                          { return macos.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return macos.RemoveProgram(executable) }
func SameProgram(a, b string) bool                    { return a == b }
func ProgramRemovalDeferred() bool                    { return false }
func PostInstall() (ServiceStatus, error)             { return macos.PostInstall() }
func RemovalUnverified(error) bool                    { return false }
