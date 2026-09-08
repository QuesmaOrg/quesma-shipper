//go:build windows

package crashjournal

import "os"

// Signal 0 is not portable here: os.Process.Signal refuses everything but Kill on Windows, so the
// POSIX probe would call every pid dead and every concurrent run a crash. os.FindProcess opens a
// handle instead, and fails when the process is gone. Errs toward alive, which suppresses a report,
// rather than toward dead, which would invent one.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
