package platform

import "syscall"

// Windows has no O_NOFOLLOW; FILE_FLAG_OPEN_REPARSE_POINT opens a symlink itself instead of its
// target, and fstat then reports it as not a regular file. Windows has no POSIX file modes either:
// Perm() is derived from the read-only attribute, so ReadPrivate skips its mode check.
const (
	openFlags                 = syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.O_CLOEXEC
	platformSupportsFileModes = false
)
