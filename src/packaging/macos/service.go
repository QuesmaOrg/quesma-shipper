//go:build darwin

package macos

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type Spec = common.Spec
type Status = common.Status

// launchdPath is the per-user LaunchAgent path, never /Library/LaunchDaemons, which is root's.
func launchdPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", bundleIdentifier+".plist")
}

// installedApp is where the package puts the bundle.
func installedApp(home string) string { return filepath.Join(home, "Applications", appName) }

func installedExecutable(home string) string {
	return filepath.Join(installedApp(home), "Contents", "MacOS", executableName)
}

// renderPlist builds the LaunchAgent. KeepAlive and RunAtLoad together are what survive both a
// dying process and a reboot; ProcessType Background keeps a poller off the interactive share.
func renderPlist(spec Spec) string {
	args := append([]string{spec.Executable}, spec.Args...)
	var argXML strings.Builder
	for _, a := range args {
		fmt.Fprintf(&argXML, "\t\t<string>%s</string>\n", escapeXML(a))
	}

	var envXML strings.Builder
	if spec.Home != "" {
		fmt.Fprintf(&envXML, "\t\t<key>HOME</key>\n\t\t<string>%s</string>\n", escapeXML(spec.Home))
	}
	if spec.StateDir != "" {
		fmt.Fprintf(&envXML, "\t\t<key>XDG_STATE_HOME</key>\n\t\t<string>%s</string>\n",
			escapeXML(filepath.Dir(spec.StateDir)))
	}

	stdout := filepath.Join(spec.LogDir, "agent.out.log")
	stderr := filepath.Join(spec.LogDir, "agent.err.log")
	// ExitTimeOut must be stated: launchd's unstated 20 seconds truncates the client's drain.
	stop := int(common.ExitTimeout(spec).Seconds())

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>%s</string>
	</array>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>ExitTimeOut</key>
	<integer>%d</integer>
	<key>ThrottleInterval</key>
	<integer>60</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, bundleIdentifier, bundleIdentifier, argXML.String(), envXML.String(), stop, escapeXML(stdout), escapeXML(stderr))
}

const launchctl = "/bin/launchctl"

func guiDomain() string  { return fmt.Sprintf("gui/%d", os.Getuid()) }
func guiService() string { return guiDomain() + "/" + bundleIdentifier }

func installService(spec Spec) error {
	path := launchdPath(spec.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("supervise: %w", err)
	}
	if spec.LogDir != "" {
		if err := os.MkdirAll(spec.LogDir, 0o700); err != nil {
			return fmt.Errorf("supervise: %w", err)
		}
	}
	// Atomic: launchd reads the plist on its own schedule and a torn one fails to load.
	if err := platform.WriteAtomic(path, []byte(renderPlist(spec)), common.EntryMode); err != nil {
		return fmt.Errorf("supervise: write %s: %w", path, err)
	}

	// bootout first, ignoring failure: otherwise re-installing keeps running the old plist.
	_ = exec.Command(launchctl, "bootout", guiService()).Run()

	// The label lingers after bootout; bootstrapping in that window fails with "5: Input/output error".
	if err := waitForLabelGone(common.ExitTimeout(spec)); err != nil {
		return fmt.Errorf("supervise: %w", err)
	}

	out, err := exec.Command(launchctl, "bootstrap", guiDomain(), path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("supervise: launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func PostInstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := common.CurrentExecutable()
	if err != nil {
		return err
	}
	expected := installedExecutable(home)
	if resolved, err := filepath.EvalSymlinks(expected); err == nil {
		expected = resolved
	}
	if exe != expected && common.HomebrewCaskRoot(exe) == "" {
		return fmt.Errorf("postinstall must run from %s, not %s", expected, exe)
	}
	if err := checkInstallOwner(exe, launchdPath(home)); err != nil {
		return err
	}
	return supervise(exe, home)
}

func checkInstallOwner(exe, plist string) error {
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	previous := common.ServiceProgram(Status{Path: plist})
	oldRoot, newRoot := common.HomebrewCaskRoot(previous), common.HomebrewCaskRoot(exe)
	if (oldRoot != "" || newRoot != "") && (oldRoot != newRoot || previous == "") {
		return fmt.Errorf("another installation owns the background service (%s); uninstall it without purging local state before changing installation methods", previous)
	}
	return nil
}

// supervise registers exe as the LaunchAgent with the default state directory: packaging cannot
// import the config layer, and the Installer environment carries no override.
func supervise(exe, home string) error {
	stateDir := filepath.Join(home, ".local", "state", "trajectory-shipper")
	spec, err := common.ServiceSpecFor(exe, stateDir, 0, 0)
	if err != nil {
		return err
	}
	if err := common.ValidateInstall(spec); err != nil {
		return err
	}
	return installService(spec)
}

func waitForLabelGone(within time.Duration) error {
	start := time.Now()
	for wait := 100 * time.Millisecond; ; wait = min(2*wait, 2*time.Second) {
		if exec.Command(launchctl, "print", guiService()).Run() != nil {
			return nil
		}
		if time.Since(start) >= within {
			return fmt.Errorf("%s still loaded after %s; `launchctl bootout %s` then re-run the installer",
				guiService(), within, guiService())
		}
		time.Sleep(wait)
	}
}

func UninstallService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := launchdPath(home)
	if exe, err := common.CurrentExecutable(); err != nil {
		return err
	} else if root := common.HomebrewCaskRoot(exe); root != "" {
		if owned, err := ownsHomebrewService(exe, path); err != nil || !owned {
			return err
		}
	}
	target := guiService()

	// Unload BEFORE removing the file, or a running agent survives with no plist to stop it.
	if out, err := exec.Command(launchctl, "bootout", target).CombinedOutput(); err != nil {
		trimmed := strings.TrimSpace(string(out))
		// "No such process" means it was not loaded, which is the state we want anyway.
		if !strings.Contains(trimmed, "No such process") && !strings.Contains(trimmed, "not find") {
			return fmt.Errorf("supervise: launchctl bootout: %w: %s", err, trimmed)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("supervise: remove %s: %w", path, err)
	}
	return nil
}

// An old cask must not stop a replacement installation during cleanup or rollback.
func ownsHomebrewService(exe, plist string) (bool, error) {
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	previous := common.ServiceProgram(Status{Path: plist})
	if previous == "" {
		return false, fmt.Errorf("cannot determine which executable owns %s; service left untouched", plist)
	}
	return previous == exe, nil
}

func ServiceState(ctx context.Context) Status {
	st := Status{Kind: common.KindLaunchd}
	home, err := os.UserHomeDir()
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	st.Path = launchdPath(home)
	if _, err := os.Stat(st.Path); err == nil {
		st.Installed = true
	}

	out, err := exec.CommandContext(ctx, launchctl, "print", guiService()).CombinedOutput()
	if err != nil {
		switch {
		case st.Installed:
			// Written but not loaded is the state that collects nothing while looking installed.
			st.Detail = "plist present but NOT loaded: re-run the Quesma Shipper installer"
		default:
			st.Detail = "no agent installed; `quesma-shipper run` works in the foreground"
		}
		return st
	}
	st.Loaded = true
	st.Detail = summariseLaunchctlPrint(string(out))
	return st
}

func RestartService(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, launchctl, "kickstart", "-k", guiService()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func RestartCommand() string {
	return "launchctl kickstart -k gui/$(id -u)/" + bundleIdentifier
}

// summariseLaunchctlPrint pulls whether a process is running and how it last exited.
func summariseLaunchctlPrint(out string) string {
	var pid, lastExit string
	for _, line := range strings.Split(out, "\n") {
		f := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(f, "pid = "); ok {
			pid = v
		} else if v, ok := strings.CutPrefix(f, "last exit code = "); ok {
			lastExit = v
		}
	}
	switch {
	case pid != "":
		return "loaded and running (pid " + pid + ")"
	case lastExit != "" && lastExit != "0":
		// A loaded agent exiting non-zero looks like success from every other angle.
		return "loaded but NOT running; last exit code " + lastExit
	default:
		return "loaded, not currently running"
	}
}

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
