//go:build darwin

package macos

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func RemoveProgram(executable string) (string, error) {
	app, ok := containingApp(executable)
	if !ok {
		if err := os.Remove(executable); err != nil && !errors.Is(err, os.ErrNotExist) {
			return executable, err
		}
		return executable, nil
	}
	if _, ok := shipperAppForExecutable(executable); !ok {
		return app, fmt.Errorf("refusing to remove %s: executable is not the %s app", app, common.Label)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return app, err
	}
	removeCLILink(filepath.Join(home, ".local", "bin", "quesma-shipper"), executable)
	removeCLILink(filepath.Join(home, ".local", "bin", "shipper"), executable)
	if err := os.RemoveAll(app); err != nil {
		return app, err
	}
	if exec.Command("/usr/sbin/pkgutil", "--volume", home, "--pkg-info", common.Label).Run() == nil {
		_ = exec.Command("/usr/sbin/pkgutil", "--volume", home, "--forget", common.Label).Run()
	}
	return app, nil
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
