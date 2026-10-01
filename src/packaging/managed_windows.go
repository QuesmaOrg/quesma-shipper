//go:build windows

package packaging

import (
	"os"

	windowspkg "github.com/QuesmaOrg/quesma-shipper/packaging/windows"
)

func ManagedRunLog(stateDir string) (*os.File, error) { return windowspkg.ManagedRunLog(stateDir) }

func ManagedEnrollment() (string, string, error) { return windowspkg.ManagedEnrollment() }
func SystemManaged() bool                        { return windowspkg.SystemManaged() }
func ValidateUserUninstall() error               { return windowspkg.ValidateUserUninstall() }
func ValidateRun() error                         { return windowspkg.ValidateRun() }
func PreUninstallSystem() error                  { return windowspkg.PreUninstallSystem() }
func PrepareUserInstall(dir string) error        { return windowspkg.PrepareUserInstall(dir) }
