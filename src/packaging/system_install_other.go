//go:build !windows

package packaging

import "errors"

var errWindowsOnly = errors.New("this setup helper is only available on Windows")

func PrepareUserInstall(string) error           { return errWindowsOnly }
func PrepareSystemInstall(string, string) error { return errWindowsOnly }
func ResumeSystemInstall(string, string) error  { return errWindowsOnly }

func SameProgram(a, b string) bool { return a == b }
func ProgramRemovalDeferred() bool { return false }
func RemovalUnverified(error) bool { return false }
