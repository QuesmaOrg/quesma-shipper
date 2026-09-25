//go:build darwin

package macos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const systemAgentPath = "/Library/LaunchAgents/" + bundleIdentifier + ".plist"

var errSystemManaged = common.ErrSystemManaged
var errRootRun = errors.New("refusing to run as root: this agent reads the current user's files")

func systemExecutable(exe string) bool { return exe == installedExecutable("/") }

func SystemManaged() bool {
	exe, _ := common.CurrentExecutable()
	if systemExecutable(exe) {
		return true
	}
	_, err := os.Lstat(systemAgentPath)
	return err == nil
}

func ValidateUserUninstall() error {
	if SystemManaged() {
		return errSystemManaged
	}
	return nil
}

func ValidateRun() error {
	if !SystemManaged() {
		return nil
	}
	if os.Geteuid() == 0 {
		return errRootRun
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return checkNoUserInstallation(home)
}

func checkAbsent(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		return fmt.Errorf("another installation exists at %s; uninstall it without purging local state before changing installation methods", path)
	}
	return nil
}

func checkNoUserInstallation(home string) error {
	return checkAbsent(installedApp(home), launchdPath(home))
}

func checkNoSystemInstallation() error {
	return checkAbsent(installedApp("/"), systemAgentPath)
}

// A shared LaunchAgent inherits each login's HOME; no identity or collection runs as root.
func renderSystemPlist() string {
	return renderPlist(Spec{Executable: installedExecutable("/"), Args: []string{"run"}, SessionType: "Aqua", Environment: map[string]string{"QUESMA_SHIPPER_SYSTEM_AGENT": "1"}})
}

type localUser struct{ uid, home string }

func localUsers() ([]localUser, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users", "UniqueID").Output()
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	homesOut, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users", "NFSHomeDirectory").Output()
	if err != nil {
		return nil, fmt.Errorf("list local user homes: %w", err)
	}
	homes := make(map[string]string)
	for _, line := range strings.Split(string(homesOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			homes[fields[0]] = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		}
	}
	var users []localUser
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		uid, err := strconv.Atoi(fields[1])
		if err != nil || uid < 500 {
			continue
		}
		home := homes[fields[0]]
		if !filepath.IsAbs(home) {
			return nil, fmt.Errorf("no absolute home for %s", fields[0])
		}
		users = append(users, localUser{uid: fields[1], home: home})
	}
	console, err := os.Stat("/dev/console")
	if err != nil {
		return nil, fmt.Errorf("read current console user: %w", err)
	}
	uid := console.Sys().(*syscall.Stat_t).Uid
	if uid >= 500 {
		id := strconv.FormatUint(uint64(uid), 10)
		for _, existing := range users {
			if existing.uid == id {
				return users, nil
			}
		}
		account, err := user.LookupId(id)
		if err != nil || !filepath.IsAbs(account.HomeDir) {
			return nil, fmt.Errorf("cannot resolve the current console user's home")
		}
		users = append(users, localUser{uid: id, home: account.HomeDir})
	}
	return users, nil
}

func postInstallSystem() error {
	if os.Geteuid() != 0 {
		return errSystemManaged
	}
	users, err := localUsers()
	if err != nil {
		return err
	}
	for _, account := range users {
		if err := checkNoUserInstallation(account.home); err != nil {
			return err
		}
	}
	if err := installCLILink("/usr/local/bin/quesma-shipper", installedExecutable("/")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(systemAgentPath), 0o755); err != nil {
		return err
	}
	if err := platform.WriteAtomic(systemAgentPath, []byte(renderSystemPlist()), 0o644); err != nil {
		return err
	}
	for _, account := range users {
		domain := "gui/" + account.uid
		if exec.Command(launchctl, "print", domain).Run() != nil {
			continue
		}
		target := domain + "/" + bundleIdentifier
		_ = exec.Command(launchctl, "bootout", target).Run()
		if err := waitForServiceGone(target, common.ExitTimeout(Spec{})); err != nil {
			return err
		}
		out, err := exec.Command(launchctl, "bootstrap", domain, systemAgentPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("start agent for %s: %w: %s", domain, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func installCLILink(path, executable string) error {
	if link, err := os.Lstat(path); err == nil {
		actual, linkErr := os.Stat(path)
		expected, exeErr := os.Stat(executable)
		if link.Mode()&os.ModeSymlink != 0 && linkErr == nil && exeErr == nil && os.SameFile(actual, expected) {
			return nil
		}
		return fmt.Errorf("another installation owns %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.Symlink(executable, path)
}

// launchd cannot expand a per-user log path in the shared plist, so the user opens it at startup.
func ManagedRunLog(stateDir string) (*os.File, error) {
	if os.Getenv("QUESMA_SHIPPER_SYSTEM_AGENT") != "1" {
		return nil, nil
	}
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("managed agent requires an absolute state directory")
	}
	dir := filepath.Join(stateDir, "logs")
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return nil, err
	}
	common.RotateLogs(dir)
	path := filepath.Join(dir, "agent.err.log")
	// NONBLOCK rejects a planted FIFO; chmod also fixes an existing log with loose permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("managed agent log is not a regular file")
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
