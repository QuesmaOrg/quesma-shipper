//go:build unix

package platform

import "syscall"

const (
	openFlags                 = syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	platformSupportsFileModes = true
)
