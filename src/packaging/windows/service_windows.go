//go:build windows

package windows

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func InstallService(spec Spec) (Status, error) {
	if err := common.ValidateInstall(spec); err != nil {
		return Status{}, err
	}
	if _, err := os.Stat(taskRunner(spec.Executable)); err != nil {
		return Status{}, fmt.Errorf("supervise: task runner beside installed program: %w", err)
	}
	current, err := user.Current()
	if err != nil {
		return Status{}, fmt.Errorf("supervise: current Windows user: %w", err)
	}
	if !strings.HasPrefix(current.Uid, "S-") {
		return Status{}, fmt.Errorf("supervise: current Windows user has invalid SID %q", current.Uid)
	}

	f, err := os.CreateTemp("", "quesma-shipper-task-*.xml")
	if err != nil {
		return Status{}, fmt.Errorf("supervise: create task definition: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(renderTask(spec, current.Uid)); err != nil {
		f.Close()
		return Status{}, fmt.Errorf("supervise: write task definition: %w", err)
	}
	if err := f.Close(); err != nil {
		return Status{}, fmt.Errorf("supervise: close task definition: %w", err)
	}

	st := Status{Kind: common.KindWindowsTask, Installed: true, Path: TaskName, Program: spec.Executable}
	if out, err := schtasks("/Create", "/TN", TaskName, "/XML", path, "/F"); err != nil {
		st.Installed = false
		st.Detail = "task registration failed: " + commandError(err, out)
		return st, fmt.Errorf("supervise: register scheduled task: %s", commandError(err, out))
	}
	if out, err := schtasks("/Run", "/TN", TaskName); err != nil {
		st.Detail = "registered but the first start failed: " + commandError(err, out)
		return st, fmt.Errorf("supervise: start scheduled task: %s", commandError(err, out))
	}
	st.Loaded = true
	st.Detail = "registered, started, and enabled at user logon"
	return st, nil
}

func UninstallService() error {
	_, _ = schtasks("/End", "/TN", TaskName)
	out, err := schtasks("/Delete", "/TN", TaskName, "/F")
	if err != nil && !taskMissing(err, out) {
		return fmt.Errorf("supervise: delete scheduled task: %s", commandError(err, out))
	}
	return nil
}

func ServiceState() Status {
	st := Status{Kind: common.KindWindowsTask, Path: TaskName}
	out, err := schtasks("/Query", "/TN", TaskName, "/XML")
	if err != nil {
		if taskMissing(err, out) {
			st.Detail = "no scheduled task installed; `quesma-shipper run` works in the foreground"
		} else {
			st.Detail = "cannot query scheduled task: " + commandError(err, out)
		}
		return st
	}
	doc, err := parseTask(out)
	if err != nil {
		st.Installed = true
		st.Detail = "scheduled task exists but its definition cannot be read: " + err.Error()
		return st
	}
	st.Installed = true
	st.Loaded = doc.Settings.Enabled
	st.Program = programFromTask(doc.Actions.Exec.Command)
	if st.Loaded {
		st.Detail = "registered and enabled at user logon"
	} else {
		st.Detail = "scheduled task is disabled: re-run the Quesma Shipper installer"
	}
	return st
}

func RestartService() error {
	_, _ = schtasks("/End", "/TN", TaskName)
	out, err := schtasks("/Run", "/TN", TaskName)
	if err != nil {
		return fmt.Errorf("restart scheduled task: %s", commandError(err, out))
	}
	return nil
}

func RestartCommand() string { return `schtasks /Run /TN "` + TaskName + `"` }

func RemoveProgram(executable string) (string, error) {
	uninstaller := filepath.Join(filepath.Dir(executable), "unins000.exe")
	if _, err := os.Stat(uninstaller); err == nil {
		cmd := exec.Command(uninstaller, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART")
		if err := cmd.Start(); err != nil {
			return uninstaller, fmt.Errorf("start Windows uninstaller: %w", err)
		}
		return uninstaller, nil
	}
	return executable, errors.New("this is a portable executable; remove it after this command exits")
}

func schtasks(args ...string) ([]byte, error) {
	return exec.Command("schtasks.exe", args...).CombinedOutput()
}

func taskMissing(err error, out []byte) bool {
	// schtasks localizes its message and collapses a missing task to exit code 1.
	var exit *exec.ExitError
	return strings.Contains(string(out), "0x80070002") ||
		(errors.As(err, &exit) && exit.ExitCode() == 1)
}

func commandError(err error, out []byte) string {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return err.Error()
	}
	return fmt.Sprintf("%v: %s", err, detail)
}
