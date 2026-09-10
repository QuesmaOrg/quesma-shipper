package windows

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// RunSupervisor keeps the replaceable shipper binary under a stable, windowless Task Scheduler
// action. The caller is built with -H windowsgui; CREATE_NO_WINDOW keeps its child windowless too.
func RunSupervisor() {
	logDir := ""
	if len(os.Args) > 1 {
		logDir = os.Args[1]
	}
	err := supervise(logDir)
	if err == nil {
		return
	}
	if f, openErr := openLog(logDir, "agent.err.log"); openErr == nil {
		fmt.Fprintf(f, "supervisor giving up: %v\n", err)
		f.Close()
	}
	os.Exit(1)
}

func supervise(logDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	child := filepath.Join(filepath.Dir(self), "quesma-shipper.exe")
	crashes := 0
	for {
		started := time.Now()
		code, err := runChild(child, logDir)
		delay, nextCrashes, restart := restartPolicy(code, time.Since(started), crashes)
		if !restart {
			if err != nil {
				return err
			}
			return errors.New("shipper repeatedly exited unexpectedly")
		}
		crashes = nextCrashes
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func runChild(path, logDir string) (int, error) {
	job, err := newKillOnCloseJob()
	if err != nil {
		return -1, err
	}
	defer windows.CloseHandle(job)

	cmd := exec.Command(path, "run")
	cmd.Env = append(os.Environ(), common.SupervisedEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	// Reopened per launch so RotateLogs can move an oversized log aside between children.
	if out, err := openLog(logDir, "agent.out.log"); err == nil {
		defer out.Close()
		cmd.Stdout = out
	}
	if errLog, err := openLog(logDir, "agent.err.log"); err == nil {
		defer errLog.Close()
		cmd.Stderr = errLog
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return -1, err
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return -1, err
	}

	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, err
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func openLog(dir, name string) (*os.File, error) {
	if dir == "" {
		return nil, errors.New("no log directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
