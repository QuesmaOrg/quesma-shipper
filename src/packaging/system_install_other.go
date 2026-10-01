//go:build !windows

package packaging

import "errors"

func PrepareUserInstall(string) error {
	return errors.New("this setup helper is only available on Windows")
}

func PrepareSystemInstall(string, string) error {
	return errors.New("this setup helper is only available on Windows")
}

func ResumeSystemInstall(string, string) error {
	return errors.New("this setup helper is only available on Windows")
}
