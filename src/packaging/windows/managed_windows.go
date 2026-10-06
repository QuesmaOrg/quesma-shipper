//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const userUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\{C65D423E-3F9B-49AF-A68F-08C88093F01F}_is1`
const managedRegistryWriteMask = uint32(registry.SET_VALUE|registry.CREATE_SUB_KEY|registry.CREATE_LINK) |
	standardDelete | standardWriteDAC | standardWriteOwner | genericWrite | genericAll

func registeredInstallDir() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ManagedInstallKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read managed installation: %w", err)
	}
	defer key.Close()
	dir, err := readRegistryString(key, ManagedInstallValue)
	if err == nil && dir == "" {
		err = errors.New("managed installation is missing its InstallDir registration; ask an administrator to repair it")
	}
	return dir, err
}

func DefaultManagedInstallDir() (string, error) {
	parent, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("locate Program Files: %w", err)
	}
	return filepath.Join(parent, "Quesma Shipper"), nil
}

func ValidateManagedInstallDir(dir string, requirePayload bool) error {
	expected, err := DefaultManagedInstallDir()
	if err != nil {
		return err
	}
	if !SameProgram(dir, expected) {
		return fmt.Errorf("managed installation must use %s", expected)
	}
	return validateManagedProgramPaths(expected, requirePayload)
}

func validateManagedProgramPaths(expected string, requirePayload bool) error {
	for _, path := range []string{filepath.Dir(expected), expected} {
		if err := protectedProgramPath(path); err != nil {
			if path == expected && !requirePayload && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	for _, name := range []string{"quesma-shipper.exe", taskRunnerName} {
		if err := protectedProgramPath(filepath.Join(expected, name)); err != nil {
			if !requirePayload && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	return nil
}

func protectedProgramPath(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		return fmt.Errorf("inspect managed program path %s: %w", path, err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("managed program path %s must not be a reparse point", path)
	}
	return protectedObject(path, windows.SE_FILE_OBJECT, writeMask)
}

// Both owners and ACLs matter: an owner can grant themselves write permission later.
func protectedObject(path string, kind windows.SE_OBJECT_TYPE, mask uint32) error {
	sd, err := windows.GetNamedSecurityInfo(path, kind, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read managed installation permissions: %w", err)
	}
	return validateManagedACL(sd, mask)
}

func validateManagedACL(sd *windows.SECURITY_DESCRIPTOR, mask uint32) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !trustedTrustee(owner.String(), "") || owner.String() == sidCreatorOwner {
		return errors.New("managed installation must be owned by administrators or SYSTEM")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount > maxACEs {
		return errors.New("managed installation has an invalid or unrestricted ACL")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var entry *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &entry); err != nil {
			return fmt.Errorf("read managed installation ACL: %w", err)
		}
		inheritOnly := entry.Header.AceFlags&inheritOnlyACE != 0
		if entry.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE ||
			(inheritOnly && entry.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) == 0) {
			continue
		}
		if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("managed installation has an unsupported ACL entry")
		}
		if uint32(entry.Mask)&mask != 0 {
			sid := (*windows.SID)(unsafe.Pointer(&entry.SidStart)).String()
			// New children must stay protected; CREATOR OWNER resolves to their privileged creator.
			if sid == sidCreatorOwner && inheritOnly {
				continue
			}
			if !trustedTrustee(sid, "") || sid == sidCreatorOwner {
				return fmt.Errorf("managed installation can be modified by non-administrator %s", sid)
			}
		}
	}
	return nil
}

func ManagedExecutable() (string, error) {
	dir, err := registeredInstallDir()
	if err != nil || dir == "" {
		return "", err
	}
	if err := validateExistingManagedRegistry(); err != nil {
		return "", err
	}
	if err := ValidateManagedInstallDir(dir, true); err != nil {
		return "", err
	}
	return filepath.Join(dir, "quesma-shipper.exe"), nil
}

func validateExistingManagedRegistry() error {
	// Windows owns the Software hive's inheritance templates; validate the application-owned subtree.
	for _, path := range []string{`Software\Quesma`, ManagedInstallKey} {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read managed installation registry parent %s: %w", path, err)
		}
		key.Close()
		if err := protectedObject(`MACHINE\`+path, windows.SE_REGISTRY_KEY, managedRegistryWriteMask); err != nil {
			return fmt.Errorf("managed installation registry parent %s: %w", path, err)
		}
	}
	return nil
}

func SystemManaged() bool {
	dir, err := registeredInstallDir()
	return err != nil || dir != "" || runningFromManagedLocation()
}

func runningFromManagedLocation() bool {
	dir, err := DefaultManagedInstallDir()
	if err != nil {
		return false
	}
	exe, err := common.CurrentExecutable()
	return err == nil && SameProgram(exe, filepath.Join(dir, "quesma-shipper.exe"))
}

func ValidateUserUninstall() error {
	dir, err := DefaultManagedInstallDir()
	if err != nil {
		return err
	}
	exe, err := common.CurrentExecutable()
	if err != nil {
		return err
	}
	if SameProgram(exe, filepath.Join(dir, "quesma-shipper.exe")) {
		return common.ErrSystemManaged
	}
	return nil
}

func validateUserIdentity() error {
	return validateProcessIdentity(validateIdentity)
}

func validateProcessIdentity(validate func(string, []string) error) error {
	token := windows.GetCurrentProcessToken()
	identity, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read collector identity: %w", err)
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return fmt.Errorf("read collector logon identity: %w", err)
	}
	var sids []string
	for _, group := range groups.AllGroups() {
		sids = append(sids, group.Sid.String())
	}
	return validate(identity.User.Sid.String(), sids)
}

func ValidateRun() error {
	managed, err := ManagedExecutable()
	if err != nil {
		return err
	}
	if managed == "" {
		if runningFromManagedLocation() {
			return errors.New("managed installation is missing its registration; ask an administrator to repair it")
		}
		return nil
	}
	if err := validateUserIdentity(); err != nil {
		return err
	}
	exe, err := common.CurrentExecutable()
	if err != nil {
		return err
	}
	if !SameProgram(exe, managed) {
		return errors.New("a machine-wide Quesma Shipper installation exists; uninstall this personal installation without purging local state")
	}
	return CheckNoUserInstallation()
}

func CheckNoUserInstallation() error {
	for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		key, err := registry.OpenKey(registry.CURRENT_USER, userUninstallKey, registry.QUERY_VALUE|view)
		if err == nil {
			key.Close()
			return errors.New("a personal Quesma Shipper installation is registered; uninstall it without purging local state before using the machine-wide installation")
		}
		if !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("check personal installation: %w", err)
		}
	}
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return err
	}
	if err := checkNoPersonalProgram(filepath.Join(local, "Programs", "Quesma Shipper")); err != nil {
		return err
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	_, out, err := queryOwnTask(context.Background(), sid)
	if err == nil {
		if _, err := parseTask(out); err != nil {
			return fmt.Errorf("check personal scheduled task: %w", err)
		}
		return errors.New("a personal Quesma Shipper scheduled task exists; uninstall it without purging local state")
	}
	return nil
}

func PrepareUserInstall(dir string) error {
	if err := validateProcessIdentity(validateInstallIdentity); err != nil {
		return err
	}
	if SystemManaged() {
		return common.ErrSystemManaged
	}
	managed, err := DefaultManagedInstallDir()
	if err != nil {
		return err
	}
	if err := checkNoManagedProgram(managed); err != nil {
		return err
	}
	if err := checkNoManagedTask(); err != nil {
		return err
	}
	if !filepath.IsAbs(dir) || SameProgram(dir, managed) || strings.HasPrefix(strings.ToLower(filepath.Clean(dir)), strings.ToLower(managed)+string(filepath.Separator)) {
		return errors.New("personal installation requires an absolute directory outside the managed installation")
	}
	current, err := currentUserSID()
	if err != nil {
		return err
	}
	for path := filepath.Clean(dir); ; path = filepath.Dir(path) {
		if _, err := os.Lstat(path); err == nil {
			return verifyInstallDir(path, current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(path) == path {
			return errors.New("personal installation has no existing parent directory")
		}
	}
}

func checkNoManagedTask() error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	// Only Task Scheduler's typed not-found HRESULT proves absence; an unreadable task blocks a scope change.
	script := `$ErrorActionPreference = 'Stop'; $s = New-Object -ComObject 'Schedule.Service'; $s.Connect(); ` +
		`$f = $s.GetFolder('\'); try { $null = $f.GetTask('Quesma Shipper Managed') } catch { ` +
		`$e = $_.Exception; while ($e.InnerException) { $e = $e.InnerException }; ` +
		`if ($e.HResult -eq -2147024894) { exit 0 }; throw }; exit 10`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(system, `WindowsPowerShell\v1.0\powershell.exe`),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("cannot verify absence of the managed scheduled task: %w", ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 10 {
		return common.ErrSystemManaged
	}
	return fmt.Errorf("cannot verify absence of the managed scheduled task: %s", commandError(err, out))
}

func ManagedRunLog(stateDir string) (*os.File, error) {
	if !SystemManaged() {
		return nil, nil
	}
	if err := validateUserIdentity(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("managed agent requires an absolute state directory")
	}
	dir := filepath.Join(stateDir, "logs")
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return nil, err
	}
	common.RotateLogs(dir)
	path := filepath.Join(dir, "agent.err.log")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(handle)
		return nil, errors.New("managed agent log is not a regular file")
	}
	return os.NewFile(uintptr(handle), path), nil
}
