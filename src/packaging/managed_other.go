//go:build !darwin

package packaging

import (
	"errors"
	"os"
)

func ManagedRunLog(string) (*os.File, error) { return nil, nil }

func ManagedEnrollment() (server, grant string, err error) { return "", "", nil }
func SystemManaged() bool                                  { return false }
func ValidateUserUninstall() error                         { return nil }
func ValidateRun() error                                   { return nil }
func PreUninstallSystem() error {
	return errors.New("system package removal is only available on macOS")
}
