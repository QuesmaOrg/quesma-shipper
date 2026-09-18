//go:build windows

package app

import (
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// PostInstallPackage registers the installed program as a per-user scheduled task, or as the
// all-users one when the machine installer put it here.
func PostInstallPackage() (string, error) {
	if packaging.ManagedInstall() {
		return "", packaging.InstallMachineService()
	}
	eff, paths, err := ResolveEffective()
	if err != nil {
		return "", err
	}
	tick, warning := config.TickInterval(eff.Schedule)
	spec, err := packaging.NewServiceSpec(paths.StateDir, eff.DrainDeadline, tick)
	if err != nil {
		return warning, err
	}
	_, err = packaging.InstallService(spec)
	return warning, err
}
