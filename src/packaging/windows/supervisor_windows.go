package windows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// RunSupervisor keeps the replaceable shipper binary under a stable, windowless Task Scheduler
// action. The caller is built with -H windowsgui; CREATE_NO_WINDOW keeps its child windowless too.
func RunSupervisor() {
	logDir := ""
	managed := len(os.Args) == 2 && os.Args[1] == "--managed"
	if len(os.Args) > 1 {
		logDir = os.Args[1]
	}
	if managed {
		var err error
		logDir, err = managedSupervisorLogDir()
		if err != nil {
			os.Exit(1)
		}
		lock, err := managedSupervisorLock()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return
		}
		if err != nil {
			if f, logErr := openLog(logDir, "agent.err.log"); logErr == nil {
				fmt.Fprintf(f, "managed supervisor: %v\n", err)
				f.Close()
			}
			os.Exit(1)
		}
		defer lock.Close()
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

func managedSupervisorLogDir() (string, error) {
	if err := validateUserIdentity(); err != nil {
		return "", err
	}
	exe, err := ManagedExecutable()
	if err != nil || exe == "" {
		return "", fmt.Errorf("locate managed program for log resolution: %v", err)
	}
	return supervisorLogDir(exe)
}

func supervisorLogDir(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "supervisor-log-dir")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, queryErr := cmd.Output()
	dir := string(out)
	if queryErr == nil && filepath.IsAbs(dir) {
		return dir, nil
	}
	// A damaged payload must still leave supervisor launch failures in the default diagnostics log.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir = platform.DefaultStateDir(home, os.LookupEnv)
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("managed supervisor requires an absolute user state directory")
	}
	return filepath.Join(dir, "logs"), nil
}

func managedSupervisorLock() (*os.File, error) {
	installed, err := ManagedExecutable()
	if err != nil {
		return nil, err
	}
	self, err := common.CurrentExecutable()
	if err != nil {
		return nil, err
	}
	if installed == "" || !SameProgram(self, taskRunner(installed)) {
		return nil, fmt.Errorf("this supervisor does not own the managed installation")
	}
	if err := CheckNoUserInstallation(); err != nil {
		return nil, err
	}
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return nil, err
	}
	return acquireSupervisorLock(filepath.Join(local, "Quesma Shipper"))
}

func acquireSupervisorLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.LockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
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
	common.RotateLogs(logDir)
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
