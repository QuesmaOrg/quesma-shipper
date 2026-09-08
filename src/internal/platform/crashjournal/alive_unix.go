//go:build !windows

package crashjournal

import (
	"os"
	"syscall"
)

// What separates a run that is merely concurrent from one that died. Signal 0 is the portable
// POSIX liveness probe: it runs the existence and permission checks and delivers nothing.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
