//go:build unix

package platform

import (
	"os"
	"syscall"
)

// LockFile takes an exclusive lock without blocking: if another process holds it, it returns an error.
func LockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func UnlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
