//go:build linux

package app

import (
	"errors"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// PostInstallPackage registers the program the Linux installer has just installed.
func PostInstallPackage() (string, error) {
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
	if errors.Is(err, packaging.ErrCronManual) {
		if warning != "" {
			warning += "\n"
		}
		return warning + "no background service; add this with `crontab -e`: " + packaging.CronHint(spec), nil
	}
	return warning, err
}
