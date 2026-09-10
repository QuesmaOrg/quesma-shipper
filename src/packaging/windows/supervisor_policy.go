package windows

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const (
	firstCrashDelay = 30 * time.Second
	maxCrashDelay   = 15 * time.Minute
	maxCrashCount   = 8
)

func restartPolicy(exitCode, crashes int) (delay time.Duration, nextCrashes int, restart bool) {
	if exitCode == common.SupervisorRestartExitCode {
		return 0, 0, true
	}
	nextCrashes = crashes + 1
	if nextCrashes >= maxCrashCount {
		return 0, nextCrashes, false
	}
	delay = firstCrashDelay << crashes
	if delay > maxCrashDelay {
		delay = maxCrashDelay
	}
	return delay, nextCrashes, true
}
