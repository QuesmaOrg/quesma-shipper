package app

import (
	"os"
	"path/filepath"

	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

type UninstallStep struct {
	Done   string
	Detail string
	Skip   string
	Err    error
}

func Uninstall(purge bool, report func(UninstallStep)) error {
	_, paths, err := ResolveEffective()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	st := packaging.ServiceState(paths.StateDir)
	switch program := packaging.ServiceProgram(st); {
	case !st.Installed:
		report(UninstallStep{Skip: "no background service to remove"})
	case program != "" && program != exe:
		report(UninstallStep{Skip: "background service left alone, it runs " + program})
	default:
		kind, err := packaging.UninstallService()
		report(UninstallStep{Done: "background service stopped and removed", Detail: string(kind), Err: err})
		if err != nil {
			return err
		}
	}
	removed, err := packaging.RemoveProgram(exe)
	if err != nil {
		report(UninstallStep{Done: "program removed", Detail: removed, Err: err})
		return err
	}
	report(UninstallStep{Done: "program removed", Detail: removed})
	return uninstallState(paths.StateDir, purge, report)
}

func uninstallState(stateDir string, purge bool, report func(UninstallStep)) error {
	if !purge {
		report(UninstallStep{Skip: "local state kept", Detail: stateDir})
		return nil
	}
	if err := packaging.RemoveState(stateDir); err != nil {
		report(UninstallStep{Done: "local state removed", Detail: stateDir, Err: err})
		return err
	}
	report(UninstallStep{Done: "local state removed", Detail: stateDir})
	return nil
}
