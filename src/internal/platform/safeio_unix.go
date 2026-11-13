//go:build unix

package platform

import (
	"os"
	"syscall"
)

const (
	openFlags                 = syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	platformSupportsFileModes = true
)

func openSpool(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
