package platform

import (
	"os"
	"syscall"
)

// Windows has no O_NOFOLLOW; FILE_FLAG_OPEN_REPARSE_POINT opens a symlink itself instead of its
// target, and fstat then reports it as not a regular file. Windows has no POSIX file modes either:
// Perm() is derived from the read-only attribute, so ReadPrivate skips its mode check.
const (
	openFlags                 = syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.O_CLOEXEC
	platformSupportsFileModes = false
)

func openSpool(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Delete-on-close also removes scrubbed plaintext when the process crashes.
	const deleteAccess = 0x00010000
	const deleteOnClose = 0x04000000
	const temporaryAttribute = 0x100
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE|deleteAccess,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.CREATE_NEW, temporaryAttribute|deleteOnClose, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
