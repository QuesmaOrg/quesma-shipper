package packaging

import "github.com/QuesmaOrg/quesma-shipper/packaging/macos"

func UninstallService() (ServiceKind, error)          { return serviceLaunchd, macos.UninstallService() }
func serviceState() ServiceStatus                     { return macos.ServiceState() }
func RestartService() error                           { return macos.RestartService() }
func RestartCommand() string                          { return macos.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return macos.RemoveProgram(executable) }
func PostInstall() (ServiceStatus, error)             { return macos.PostInstall() }
