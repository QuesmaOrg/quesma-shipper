//go:build darwin

package macos

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	"golang.org/x/sys/unix"
)

const macPackageTarget = "darwin/pkg"

func appUpdateTarget(release common.Release) string { return release.Targets[macPackageTarget] }

func currentAppBundle() (string, bool) {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return "", false
	}
	return appForExecutable(exe)
}

func containingApp(exe string) (string, bool) {
	macOS := filepath.Dir(exe)
	contents := filepath.Dir(macOS)
	app := filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(app) != ".app" {
		return "", false
	}
	return app, true
}

func appForExecutable(exe string) (string, bool) {
	app, ok := containingApp(exe)
	if !ok || filepath.Base(app) != appName || filepath.Base(exe) != executableName {
		return "", false
	}
	return app, bundleIdentifierOf(app) == bundleIdentifier
}

func bundleIdentifierOf(app string) string {
	plist := filepath.Join(app, "Contents", "Info.plist")
	if _, err := os.Stat(plist); err != nil {
		return ""
	}
	out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// applyAppPackage swaps the bundle in whole. No bundle at app yet (the rename bridge laying the
// renamed one out for the first time) is a plain move.
func applyAppPackage(raw []byte, app, version string) error {
	stage, stagedApp, err := stageAppPackage(raw, filepath.Dir(app), version)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	err = unix.RenamexNp(app, stagedApp, unix.RENAME_SWAP)
	if errors.Is(err, unix.ENOENT) {
		err = os.Rename(stagedApp, app)
	}
	if err != nil {
		return fmt.Errorf("replacing %s: %w", app, err)
	}
	return nil
}

// stageAppPackage expands the package under parent and validates the bundle inside; the caller
// removes stage once done with stagedApp.
func stageAppPackage(raw []byte, parent, version string) (string, string, error) {
	stage, err := os.MkdirTemp(parent, ".quesma-shipper-update-")
	if err != nil {
		return "", "", fmt.Errorf("staging in %s: %w", parent, err)
	}
	stagedApp, err := expandAppPackage(raw, stage, version)
	if err != nil {
		os.RemoveAll(stage)
		return "", "", err
	}
	return stage, stagedApp, nil
}

func expandAppPackage(raw []byte, stage, version string) (string, error) {
	expanded, err := expandPackage(raw, stage)
	if err != nil {
		return "", err
	}
	stagedApp := filepath.Join(expanded, componentPackage, "Payload", "Applications", appName)
	if err := validateAppBundle(stagedApp, version); err != nil {
		return "", err
	}
	return stagedApp, nil
}

// expandPackage unpacks the product archive under stage, every component included.
func expandPackage(raw []byte, stage string) (string, error) {
	pkg := filepath.Join(stage, "quesma-shipper.pkg")
	if err := platform.WriteAtomic(pkg, raw, 0o600); err != nil {
		return "", fmt.Errorf("writing staged package: %w", err)
	}
	expanded := filepath.Join(stage, "expanded")
	if out, err := exec.Command("/usr/sbin/pkgutil", "--expand-full", pkg, expanded).CombinedOutput(); err != nil {
		return "", fmt.Errorf("extracting package: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return expanded, nil
}

func validateAppBundle(app, version string) error {
	info, err := os.Lstat(app)
	if err != nil {
		return fmt.Errorf("update does not contain %s: %w", appName, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("update %s is not a directory", appName)
	}
	plist := filepath.Join(app, "Contents", "Info.plist")
	checks := map[string]string{
		"CFBundleIdentifier": bundleIdentifier,
		releaseVersionField:  version,
	}
	for key, want := range checks {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("reading %s from update: %w: %s", key, err, strings.TrimSpace(string(out)))
		}
		if got := strings.TrimSpace(string(out)); got != want {
			return fmt.Errorf("update %s is %q, want %q", key, got, want)
		}
	}
	executable := filepath.Join(app, "Contents", "MacOS", executableName)
	if info, err := os.Stat(executable); err != nil || info.Mode()&0o111 == 0 {
		return fmt.Errorf("update has no executable Contents/MacOS/%s", executableName)
	}
	return nil
}
