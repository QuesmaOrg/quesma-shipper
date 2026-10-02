package sqliteread

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
	sqlite3 "modernc.org/sqlite/lib"
)

const sessionVFS = "win32"

func verifySessionHandle(p *sessionPath, h windows.Handle, suffix string) error {
	info, err := p.parent.Lstat(p.name + suffix)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session database%s is not regular", suffix)
	}
	f, err := p.parent.OpenFile(p.name+suffix, int(windows.FILE_FLAG_OPEN_REPARSE_POINT), 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || suffix == "" && !os.SameFile(p.info, opened) {
		return fmt.Errorf("session database%s changed while opening", suffix)
	}
	var actual, expected windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &actual); err != nil {
		return err
	}
	if err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &expected); err != nil {
		return err
	}
	if expected.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("session sidecar is a reparse point")
	}
	if actual.VolumeSerialNumber != expected.VolumeSerialNumber || actual.FileIndexHigh != expected.FileIndexHigh || actual.FileIndexLow != expected.FileIndexLow {
		return fmt.Errorf("session database%s changed while opening", suffix)
	}
	return nil
}

func verifySessionFile(p *sessionPath, file uintptr, suffix string) error {
	if file == 0 {
		return fmt.Errorf("session database has no native file")
	}
	return verifySessionHandle(p, windows.Handle(nativeValue[sqlite3.TwinFile](file).Fh), suffix)
}

func verifySessionSHM(p *sessionPath, file uintptr) error {
	shm := nativeValue[sqlite3.TwinFile](file).FpShm
	if shm == 0 {
		return nil
	}
	connection := nativeValue[sqlite3.TwinShm](shm)
	node := nativeValue[sqlite3.TwinShmNode](connection.FpShmNode)
	if err := verifySessionHandle(p, windows.Handle(node.FhSharedShm), "-shm"); err != nil {
		return err
	}
	if node.FbUseSharedLockHandle == 0 {
		return verifySessionHandle(p, windows.Handle(connection.FhShm), "-shm")
	}
	return nil
}
