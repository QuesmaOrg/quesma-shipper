//go:build darwin

// Rename-bridge glue for installs made as Shipper.app. Their updater accepts the new package only
// because it also carries a Shipper-component.pkg (see build-pkg.sh); the binary it swaps in then
// moves the install to the renamed layout on its first start. Delete this file, its callers and
// the bridge component once no agent older than the rename remains.
package macos

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const (
	legacyAppName          = "Shipper.app"
	legacyExecutableName   = "shipper"
	legacyBundleIdentifier = "com.quesma.trajectory-shipper"
)

// bootoutLegacy is a variable so tests never reach a developer's real launchd domain.
var bootoutLegacy = func(wait bool) {
	cmd := exec.Command(launchctl, "bootout", guiServiceFor(legacyBundleIdentifier))
	if wait {
		_ = cmd.Run()
		return
	}
	_ = cmd.Start()
}

func legacyAppPath(home string) string { return filepath.Join(home, "Applications", legacyAppName) }

func legacyAppForExecutable(exe string) (string, bool) {
	app, ok := containingApp(exe)
	if !ok || filepath.Base(exe) != legacyExecutableName {
		return "", false
	}
	return app, bundleIdentifierOf(app) == legacyBundleIdentifier
}

// MigrateLegacyInstall lays the renamed install out beside the Shipper.app this process runs from,
// starts its agent and retires the former one. True means this process was the former agent and
// is done; launchd normally ends it before the return.
func MigrateLegacyInstall(ctx context.Context, out io.Writer) (bool, error) {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return false, err
	}
	legacyApp, ok := legacyAppForExecutable(exe)
	if !ok {
		return false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	app, target := installedApp(home), installedExecutable(home)
	if _, ok := appForExecutable(target); !ok {
		fmt.Fprintf(out, "rename migration: laying out %s\n", app)
		raw, version, err := common.Fetch(ctx, common.Options{Out: out}, appUpdateTarget)
		if err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
			return false, err
		}
		if err := applyAppPackage(raw, app, version); err != nil {
			return false, err
		}
	}
	linkCLI(filepath.Join(home, ".local", "bin", executableName),
		filepath.Join("..", "..", "Applications", appName, "Contents", "MacOS", executableName))
	if !ServiceState().Loaded {
		if _, err := supervise(target, home); err != nil {
			return false, err
		}
	}
	fmt.Fprintf(out, "rename migration: %s took over; retiring %s\n", app, legacyApp)
	retireLegacyInstall(home, legacyApp, true)
	return true, nil
}

// linkCLI points link at target, replacing only a symlink: a regular file there is someone else's.
func linkCLI(link, target string) {
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	_ = os.Remove(link)
	_ = os.Symlink(target, link)
}

// retireLegacyInstall removes what the former package left behind: its CLI link, app, receipt and
// LaunchAgent. Booting the former label out comes last because it ends the former agent, which is
// this very process when self is set.
func retireLegacyInstall(home, legacyApp string, self bool) {
	removeLegacyLink(filepath.Join(home, ".local", "bin", legacyExecutableName))
	if _, ok := legacyAppForExecutable(filepath.Join(legacyApp, "Contents", "MacOS", legacyExecutableName)); ok {
		_ = os.RemoveAll(legacyApp)
	}
	forgetReceipt(home, legacyBundleIdentifier)
	if err := os.Remove(launchdPathFor(home, legacyBundleIdentifier)); err == nil {
		bootoutLegacy(!self)
	}
}

// removeLegacyLink drops ~/.local/bin/shipper when it points into a Shipper.app, dangling or not.
func removeLegacyLink(link string) {
	inside := filepath.Join("Applications", legacyAppName, "Contents", "MacOS", legacyExecutableName)
	if target, err := os.Readlink(link); err == nil && strings.HasSuffix(target, inside) {
		_ = os.Remove(link)
	}
}
