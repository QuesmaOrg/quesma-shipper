//go:build !darwin

package packaging

import "os"

func ManagedRunLog(string) (*os.File, error) { return nil, nil }

func ManagedEnrollment() (server, grant string, err error) { return "", "", nil }
func SystemManaged() bool                                  { return false }
func ValidateUserUninstall() error                         { return nil }
func ValidateRun() error                                   { return nil }
