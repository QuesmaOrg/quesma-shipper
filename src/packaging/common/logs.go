package common

import (
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// agentLogs are what launchd redirects the process's output into; journald bounds systemd's.
var agentLogs = []string{"agent.out.log", "agent.err.log"}

// RotateLogs bounds a crash loop appending the same stack: it moves an oversized agent log aside at
// startup, which works because the supervisor reopens the path per launch.
func RotateLogs(logDir string) {
	if logDir == "" {
		return
	}
	for _, name := range agentLogs {
		platform.RotateLog(filepath.Join(logDir, name))
	}
}
