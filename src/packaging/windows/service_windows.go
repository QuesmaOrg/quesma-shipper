//go:build windows

package windows

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func InstallService(spec Spec) error {
	if err := common.ValidateInstall(spec); err != nil {
		return err
	}
	if _, err := os.Stat(taskRunner(spec.Executable)); err != nil {
		return fmt.Errorf("supervise: task runner beside installed program: %w", err)
	}
	current, err := currentUser()
	if err != nil {
		return err
	}
	if err := verifyInstallDir(filepath.Dir(spec.Executable), current.Uid); err != nil {
		return err
	}
	// Retired before the new task is registered: the two would otherwise both be live, and the
	// second shipper would spend its life failing to take the state lock.
	if err := retireLegacyTask(current.Uid); err != nil {
		return err
	}
	name := taskName(current.Uid)

	f, err := os.CreateTemp("", "quesma-shipper-task-*.xml")
	if err != nil {
		return fmt.Errorf("supervise: create task definition: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(taskXMLForSchtasks(renderTask(spec, current.Uid, current.Username))); err != nil {
		f.Close()
		return fmt.Errorf("supervise: write task definition: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("supervise: close task definition: %w", err)
	}

	// /F is an overwrite of this user's own task now that the name carries their SID.
	ctx := context.Background()
	if out, err := schtasksContext(ctx, "/Create", "/TN", name, "/XML", path, "/F"); err != nil {
		return fmt.Errorf("supervise: register scheduled task: %s", commandError(err, out))
	}
	if out, err := schtasksContext(ctx, "/Run", "/TN", name); err != nil {
		return fmt.Errorf("supervise: start scheduled task: %s", commandError(err, out))
	}
	return nil
}

func UninstallService() error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	name := taskName(sid)
	// An install that predates per-user names is removed too, or uninstalling would leave it live.
	legacyErr := retireLegacyTask(sid)

	ctx := context.Background()
	_, _ = schtasksContext(ctx, "/End", "/TN", name)
	out, err := schtasksContext(ctx, "/Delete", "/TN", name, "/F")
	if err == nil {
		return legacyErr
	}
	exists, verifyErr := taskExists(ctx, name)
	if verifyErr == nil && !exists {
		return legacyErr
	}
	if verifyErr != nil {
		return fmt.Errorf("%w: %s (could not verify absence: %v)",
			ErrTaskDeleteUnverified, commandError(err, out), verifyErr)
	}
	return fmt.Errorf("supervise: delete scheduled task: %s", commandError(err, out))
}

// retireLegacyTask removes the pre-rename task, and only when this user owns it. A failure to read
// it is not an error: it is either absent, or another user's and therefore invisible to us.
func retireLegacyTask(userSID string) error {
	ctx := context.Background()
	if _, ok := ownedLegacyTask(ctx, userSID); !ok {
		return nil
	}
	_, _ = schtasksContext(ctx, "/End", "/TN", legacyTaskName)
	if out, err := schtasksContext(ctx, "/Delete", "/TN", legacyTaskName, "/F"); err != nil {
		return fmt.Errorf("supervise: retire the former scheduled task %q: %s",
			legacyTaskName, commandError(err, out))
	}
	return nil
}

func currentUser() (*user.User, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("supervise: current Windows user: %w", err)
	}
	if !strings.HasPrefix(current.Uid, "S-") {
		return nil, fmt.Errorf("supervise: current Windows user has invalid SID %q", current.Uid)
	}
	return current, nil
}

func currentUserSID() (string, error) {
	current, err := currentUser()
	if err != nil {
		return "", err
	}
	return current.Uid, nil
}

// queryOwnTask prefers this user's own task and falls back to the pre-rename one, so an install
// made before the rename still reports as installed until its next upgrade migrates it. The
// per-user name comes back even when nothing is registered: it is what a fresh install will use.
func queryOwnTask(ctx context.Context, userSID string) (string, []byte, error) {
	name := taskName(userSID)
	out, err := schtasksContext(ctx, "/Query", "/TN", name, "/XML")
	if err == nil {
		return name, out, nil
	}
	if legacyOut, ok := ownedLegacyTask(ctx, userSID); ok {
		return legacyTaskName, legacyOut, nil
	}
	return name, out, err
}

// ownedLegacyTask reads the pre-rename task's definition; ok is false when it is absent,
// unreadable, or another user's.
func ownedLegacyTask(ctx context.Context, userSID string) (out []byte, ok bool) {
	out, err := schtasksContext(ctx, "/Query", "/TN", legacyTaskName, "/XML")
	if err != nil {
		return nil, false
	}
	doc, err := parseTask(out)
	if err != nil || !legacyTaskIsOurs(doc, userSID) {
		return nil, false
	}
	return out, true
}

// ErrTaskDeleteUnverified marks a delete whose outcome could not be confirmed either way. Removal
// must not be blocked by it: a user who wants the software gone has to be able to get there.
var ErrTaskDeleteUnverified = errors.New("supervise: delete scheduled task, outcome unverified")

func ServiceState(ctx context.Context) Status {
	sid, err := currentUserSID()
	if err != nil {
		return Status{Kind: common.KindWindowsTask, Detail: err.Error()}
	}
	name, out, err := queryOwnTask(ctx, sid)
	st := Status{Kind: common.KindWindowsTask, Path: name}
	if err != nil {
		exists, verifyErr := taskExists(ctx, name)
		if verifyErr == nil && !exists {
			st.Detail = "no scheduled task installed; `quesma-shipper run` works in the foreground"
		} else {
			st.Detail = "cannot query scheduled task: " + commandError(err, out)
			if verifyErr != nil {
				st.Detail += "; cannot enumerate tasks: " + verifyErr.Error()
			} else {
				st.Installed = true
			}
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
	st.Loaded = doc.enabled()
	st.Program = programFromTask(doc.Actions.Exec.Command)
	if st.Loaded {
		st.Detail = "registered and enabled at user logon"
	} else {
		st.Detail = "scheduled task is disabled: re-run the Quesma Shipper installer"
	}
	return st
}

func RestartService(ctx context.Context) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	name, _, _ := queryOwnTask(ctx, sid)
	_, _ = schtasksContext(ctx, "/End", "/TN", name)
	out, err := schtasksContext(ctx, "/Run", "/TN", name)
	if err != nil {
		return fmt.Errorf("restart scheduled task: %s", commandError(err, out))
	}
	return nil
}

// RestartCommand is a hint printed for the user; the caller drops it when it is empty.
func RestartCommand() string {
	sid, err := currentUserSID()
	if err != nil {
		return ""
	}
	name, _, _ := queryOwnTask(context.Background(), sid)
	return `schtasks /Run /TN "` + name + `"`
}

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

func SameProgram(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func schtasksContext(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
}

// schtasksStdout keeps a machine-readable listing clear of the per-task warnings schtasks writes
// to stderr when it meets a task it cannot read.
func schtasksStdout(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "schtasks.exe", args...).Output()
}

// taskExists enumerates all tasks after a targeted operation failed. A successful enumeration can
// prove absence without interpreting schtasks' localized text or its catch-all exit code 1.
func taskExists(ctx context.Context, name string) (bool, error) {
	out, err := schtasksStdout(ctx, "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return false, errors.New(commandError(err, out))
	}
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("parse task enumeration: %w", err)
		}
		if len(record) > 0 && strings.EqualFold(strings.TrimSpace(record[0]), name) {
			return true, nil
		}
	}
}

func commandError(err error, out []byte) string {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return err.Error()
	}
	return fmt.Sprintf("%v: %s", err, detail)
}
