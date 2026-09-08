package packaging

import (
	"errors"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	linuxpkg "github.com/QuesmaOrg/quesma-shipper/packaging/linux"
)

type ServiceSpec = common.Spec

var ErrCronManual = common.ErrCronManual

func NewServiceSpec(stateDir string, stopTimeout, tick time.Duration) (ServiceSpec, error) {
	return common.NewServiceSpec(stateDir, stopTimeout, tick)
}

func CronHint(spec ServiceSpec) string { return common.CronHint(spec) }

func detectService() ServiceKind {
	if linuxpkg.Available() {
		return serviceSystemd
	}
	return ServiceCron
}

func InstallService(spec ServiceSpec) (ServiceStatus, error) {
	if err := common.ValidateInstall(spec); err != nil {
		return ServiceStatus{}, err
	}
	if detectService() == ServiceCron {
		return ServiceStatus{Kind: ServiceCron}, ErrCronManual
	}
	return linuxpkg.InstallService(spec)
}

func UninstallService() (ServiceKind, error) {
	if detectService() == ServiceCron {
		return ServiceCron, errors.New("supervise: nothing to remove: this host was never given a supervision entry, only a crontab line to add by hand")
	}
	return serviceSystemd, linuxpkg.UninstallService()
}

func serviceState() ServiceStatus {
	if detectService() == ServiceCron {
		return ServiceStatus{Kind: ServiceCron, Detail: "no supervision entry: this host uses cron, which the client does not edit"}
	}
	return linuxpkg.ServiceState()
}

func RestartService() error                           { return linuxpkg.RestartService() }
func RestartCommand() string                          { return linuxpkg.RestartCommand() }
func RemoveProgram(executable string) (string, error) { return common.RemoveProgram(executable) }
