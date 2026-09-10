package windows

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const (
	firstCrashDelay = 30 * time.Second
	// A child that stayed up this long proves the install works, so the crash budget resets.
	healthyUptime = 10 * time.Minute
	maxCrashDelay = 15 * time.Minute
	maxCrashCount = 8
)

func restartPolicy(exitCode int, uptime time.Duration, crashes int) (delay time.Duration, nextCrashes int, restart bool) {
	if exitCode == common.SupervisorRestartExitCode {
		return 0, 0, true
	}
	if uptime >= healthyUptime {
		crashes = 0
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
