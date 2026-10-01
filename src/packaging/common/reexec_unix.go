//go:build unix

package common

import (
	"os"
	"syscall"
)

// Linux follows the running inode after an update renames and removes the old executable.
var reexecPath, reexecPathErr = os.Executable()

// ReExec replaces this process with the binary now on disk at this executable's path, keeping argv
// and the environment; it never returns on success. No loop protection: the caller sets its guard.
func ReExec() error {
	if reexecPathErr != nil {
		return reexecPathErr
	}
	return syscall.Exec(reexecPath, os.Args, os.Environ())
}
