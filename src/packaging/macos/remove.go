//go:build darwin

package macos

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func RemoveProgram(executable string) (string, error) {
	app, ok := containingApp(executable)
	if !ok {
		if err := os.Remove(executable); err != nil && !errors.Is(err, os.ErrNotExist) {
			return executable, err
		}
		return executable, nil
	}
	if _, ok := appForExecutable(executable); !ok {
		return app, fmt.Errorf("refusing to remove %s: executable is not %s", app, appName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return app, err
	}
	removeCLILink(filepath.Join(home, ".local", "bin", executableName), executable)
	if err := os.RemoveAll(app); err != nil {
		return app, err
	}
	forgetReceipt(home)
	return app, nil
}

func forgetReceipt(home string) {
	_ = exec.Command("/usr/sbin/pkgutil", "--volume", home, "--forget", bundleIdentifier).Run()
}

func removeCLILink(path, executable string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return
	}
	linkInfo, linkErr := os.Stat(path)
	exeInfo, exeErr := os.Stat(executable)
	if linkErr == nil && exeErr == nil && os.SameFile(linkInfo, exeInfo) {
		_ = os.Remove(path)
	}
}
