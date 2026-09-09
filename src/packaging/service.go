package packaging

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type ServiceKind = common.Kind
type ServiceStatus = common.Status

const (
	serviceLaunchd     = common.KindLaunchd
	serviceSystemd     = common.KindSystemd
	serviceWindowsTask = common.KindWindowsTask
	ServiceCron        = common.KindCron
	serviceUnsupported = common.KindUnsupported
)

func ServiceState(stateDir string) ServiceStatus {
	st := serviceState()
	st.LastRun = common.LastRun(stateDir)
	return st
}

func RecordRun(stateDir string, at time.Time) error { return common.RecordRun(stateDir, at) }
func RotateLogs(logDir string)                      { common.RotateLogs(logDir) }
func ServiceProgram(st ServiceStatus) string        { return common.ServiceProgram(st) }
func RemoveState(stateDir string) error             { return common.RemoveState(stateDir) }
