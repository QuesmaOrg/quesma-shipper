package common

import (
	"os"
	"os/exec"
)

const supervisedEnv = "SHIPPER_SUPERVISED"

// No exec(2) on Windows: start the new binary and leave; waiting would keep a stale parent alive.
func ReExec() error {
	if os.Getenv(supervisedEnv) != "" {
		// The installed runner starts the replaced binary without letting it escape supervision.
		os.Exit(75)
	}
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
