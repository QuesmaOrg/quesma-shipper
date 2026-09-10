//go:build windows

package packaging

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	windowspkg "github.com/QuesmaOrg/quesma-shipper/packaging/windows"
)

type ServiceSpec = common.Spec

func NewServiceSpec(stateDir string, stopTimeout, tick time.Duration) (ServiceSpec, error) {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return ServiceSpec{}, err
	}
	return common.ServiceSpecFor(exe, stateDir, stopTimeout, tick)
}

func InstallService(spec ServiceSpec) (ServiceStatus, error) { return windowspkg.InstallService(spec) }
func UninstallService() (ServiceKind, error) {
	return serviceWindowsTask, windowspkg.UninstallService()
}
func serviceState() ServiceStatus                     { return windowspkg.ServiceState() }
func RestartService() error                           { return windowspkg.RestartService() }
func RestartCommand() string                          { return windowspkg.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return windowspkg.RemoveProgram(executable) }
func SameProgram(a, b string) bool                    { return windowspkg.SameProgram(a, b) }
func ProgramRemovalDeferred() bool                    { return windowspkg.ProgramRemovalDeferred() }
