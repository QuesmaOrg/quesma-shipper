//go:build unix

package common

import (
	"os"
	"syscall"
)

// Capture the path at startup; on Linux, the updater's rename makes os.Executable return the deleted backup path.
var reexecPath, _ = os.Executable()

// ReExec replaces this process with the binary now on disk at this executable's path, keeping argv
// and the environment; it never returns on success. No loop protection: the caller sets its guard.
func ReExec() error {
	return syscall.Exec(reexecPath, os.Args, os.Environ())
}
