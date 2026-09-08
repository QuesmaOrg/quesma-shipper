package common

import (
	"os"
	"os/exec"
)

// No exec(2) on Windows: start the new binary and leave; waiting would keep a stale parent alive.
func ReExec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
