package platform

import "sync"

// OS release and boot time for the control-plane headers: read once per process, best-effort —
// a platform where the probe fails reports "" and the dashboard shows a blank.

var (
	osVersion = sync.OnceValue(readOSVersion)
	bootTime  = sync.OnceValue(readBootTime)
)

// OSVersion is the human-readable OS release ("macOS 26.5.1", "Ubuntu 24.04.2 LTS"), or "".
func OSVersion() string { return osVersion() }

// BootTime is the machine's last boot as RFC3339 UTC, or "" when unknown.
func BootTime() string { return bootTime() }
