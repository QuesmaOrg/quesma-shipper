//go:build darwin || linux

package sqliteread

import (
	"fmt"
	"syscall"

	sqlite3 "modernc.org/sqlite/lib"
)

const sessionVFS = "unix"

func verifySessionFD(p *sessionPath, fd int, suffix string) error {
	expected, err := p.parent.Lstat(p.name + suffix)
	if err != nil {
		return err
	}
	if suffix == "" {
		expected = p.info
	}
	var actual syscall.Stat_t
	if err := syscall.Fstat(fd, &actual); err != nil {
		return err
	}
	st, ok := expected.Sys().(*syscall.Stat_t)
	if !expected.Mode().IsRegular() || !ok || st.Dev != actual.Dev || st.Ino != actual.Ino {
		return fmt.Errorf("session database%s changed while opening", suffix)
	}
	return nil
}

func verifySessionFile(p *sessionPath, file uintptr, suffix string) error {
	if file == 0 {
		return fmt.Errorf("session database has no native file")
	}
	return verifySessionFD(p, int(nativeValue[sqlite3.TunixFile](file).Fh), suffix)
}

func verifySessionSHM(p *sessionPath, file uintptr) error {
	shm := nativeValue[sqlite3.TunixFile](file).FpShm
	if shm == 0 {
		return nil
	}
	node := nativeValue[sqlite3.TunixShm](shm).FpShmNode
	return verifySessionFD(p, int(nativeValue[sqlite3.TunixShmNode](node).FhShm), "-shm")
}
