//go:build unix

package common

import (
	"os"
	"syscall"
)

// ReExec replaces this process with the binary now on disk at this executable's path, keeping argv
// and the environment; it never returns on success. No loop protection: the caller sets its guard.
func ReExec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
