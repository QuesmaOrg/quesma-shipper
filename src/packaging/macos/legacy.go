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

// MigrateLegacyInstall moves an install made under the former name over to the renamed one and
// retires the former agent. True means this process was that agent and is done; launchd normally
// ends it before the return. Two shapes exist: a Shipper.app bundle, and a raw binary named
// shipper that self-updated in place under the former label.
func MigrateLegacyInstall(ctx context.Context, out io.Writer) (bool, error) {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	if legacyApp, ok := legacyAppForExecutable(exe); ok {
		return migrateLegacyApp(ctx, out, home, legacyApp)
	}
	if isLegacyBinary(exe) {
		return migrateLegacyBinary(out, home, exe)
	}
	return false, nil
}

// isLegacyBinary is the raw shape: the former name outside any bundle.
func isLegacyBinary(exe string) bool {
	_, inApp := containingApp(exe)
	return !inApp && filepath.Base(exe) == legacyExecutableName
}

// migrateLegacyBinary renames the file in place, like the Linux bridge, and moves a LaunchAgent
// under the former label onto the new one. Without that plist the rename is all there is to do.
func migrateLegacyBinary(out io.Writer, home, exe string) (bool, error) {
	renamed := filepath.Join(filepath.Dir(exe), executableName)
	if err := os.Rename(exe, renamed); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "rename migration: %s is now %s\n", exe, renamed)
	if _, err := os.Stat(launchdPathFor(home, legacyBundleIdentifier)); err != nil {
		fmt.Fprintf(out, "rename migration: no former LaunchAgent; whatever starts this must now name %s\n", renamed)
		return false, nil
	}
	if _, err := supervise(renamed, home); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "rename migration: %s took over under %s; retiring %s\n", renamed, bundleIdentifier, legacyBundleIdentifier)
	retireLegacyInstall(home, legacyAppPath(home), true)
	return true, nil
}

func migrateLegacyApp(ctx context.Context, out io.Writer, home, legacyApp string) (bool, error) {
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
