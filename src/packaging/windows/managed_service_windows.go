package windows

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const managedTaskName = `\Quesma Shipper Managed`

//go:embed managed_tasks.ps1
var managedTasksScript string

func requireAdministrator() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("machine installation and removal require administrator privileges")
	}
	return nil
}

func PrepareSystemInstall(dir, recoveryFile string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	if err := validateExistingManagedRegistry(); err != nil {
		return err
	}
	if err := ValidateManagedInstallDir(dir, false); err != nil {
		return err
	}
	return managedTaskAction("Prepare", dir, recoveryFile)
}

func ResumeSystemInstall(dir, recoveryFile string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	if err := ValidateManagedInstallDir(dir, false); err != nil {
		return err
	}
	return managedTaskAction("Resume", dir, recoveryFile)
}

func PostInstallSystem() error {
	dir, err := currentManagedDirectory()
	if err != nil {
		return err
	}
	return managedTaskAction("Install", dir, "")
}

// The hidden preuninstall-system CLI uses this; Inno removes damaged payloads independently.
func PreUninstallSystem() error {
	dir, err := currentManagedDirectory()
	if err != nil {
		return err
	}
	return managedTaskAction("Remove", dir, "")
}

func currentManagedDirectory() (string, error) {
	if err := requireAdministrator(); err != nil {
		return "", err
	}
	exe, err := common.CurrentExecutable()
	if err != nil {
		return "", err
	}
	installed, err := ManagedExecutable()
	if err != nil {
		return "", err
	}
	if installed == "" || !SameProgram(installed, exe) {
		return "", fmt.Errorf("this executable does not own the managed installation")
	}
	return filepath.Dir(installed), nil
}

func managedTaskAction(action, dir, recoveryFile string) error {
	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	script := "& {\n" + managedTasksScript + "\n} -Operation " + psQuote(action) +
		" -InstallDir " + psQuote(dir) + " -RecoveryFile " + psQuote(recoveryFile)
	// A single encoded argument preserves paths containing quotes without invoking a command shell.
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(units))
	for i, unit := range units {
		raw[2*i], raw[2*i+1] = byte(unit), byte(unit>>8)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(systemDir, `WindowsPowerShell\v1.0\powershell.exe`),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("managed installation %s: %s", strings.ToLower(action), commandError(err, out))
	}
	if len(out) != 0 {
		fmt.Fprint(os.Stderr, string(out))
	}
	return nil
}

func psQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func managedServiceState(ctx context.Context) Status {
	st := Status{Kind: common.KindWindowsTask, Path: managedTaskName, Installed: true}
	exe, err := ManagedExecutable()
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	st.Program = exe
	out, err := schtasksContext(ctx, "/Query", "/TN", managedTaskName, "/XML")
	if err != nil {
		st.Detail = "managed task cannot be queried; ask an administrator to repair the installation"
		return st
	}
	doc, err := parseTask(out)
	if err != nil || !SameProgram(doc.Actions.Exec.Command, taskRunner(exe)) ||
		doc.Actions.Exec.Arguments != "--managed" || !isUsersGroup(doc.Principals.Principal.GroupID) ||
		!doc.leastPrivilege() {
		st.Detail = "managed task does not match the installation; ask an administrator to repair it"
		return st
	}
	st.Loaded = doc.enabled()
	if st.Loaded {
		st.Detail = "managed task enabled for interactive users; collection and enrollment are per user"
	} else {
		st.Detail = "managed task disabled; ask an administrator to repair the installation"
	}
	return st
}

func isUsersGroup(value string) bool {
	sid, err := windows.StringToSid(value)
	if err != nil {
		sid, _, _, err = windows.LookupSID("", value)
	}
	return err == nil && sid.IsWellKnown(windows.WinBuiltinUsersSid)
}
