//go:build darwin

package macos

import (
	"bytes"
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
	return filepath.Join(home, "Library", "LaunchAgents", common.Label+".plist")
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
`, common.Label, common.Label, argXML.String(), envXML.String(), stop, escapeXML(stdout), escapeXML(stderr))
}

const launchctl = "/bin/launchctl"

func guiDomain() string  { return fmt.Sprintf("gui/%d", os.Getuid()) }
func guiService() string { return guiDomain() + "/" + common.Label }

func installService(spec Spec) (Status, error) {
	home, err := common.HomeFor(spec)
	if err != nil {
		return Status{}, err
	}
	path := launchdPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Status{}, fmt.Errorf("supervise: %w", err)
	}
	if spec.LogDir != "" {
		if err := os.MkdirAll(spec.LogDir, 0o700); err != nil {
			return Status{}, fmt.Errorf("supervise: %w", err)
		}
	}
	// Atomic: launchd reads the plist on its own schedule and a torn one fails to load.
	if err := platform.WriteAtomic(path, []byte(renderPlist(spec)), common.EntryMode); err != nil {
		return Status{}, fmt.Errorf("supervise: write %s: %w", path, err)
	}

	// bootout first, ignoring failure: otherwise re-installing keeps running the old plist.
	_ = exec.Command(launchctl, "bootout", guiService()).Run()

	// The label lingers after bootout; bootstrapping in that window fails with "5: Input/output error".
	st := Status{Kind: common.KindLaunchd, Installed: true, Path: path}
	if err := waitForLabelGone(common.ExitTimeout(spec)); err != nil {
		st.Detail = "plist written but the previous agent did not exit: " + err.Error()
		return st, fmt.Errorf("supervise: %w", err)
	}

	out, err := exec.Command(launchctl, "bootstrap", guiDomain(), path).CombinedOutput()
	if err != nil {
		st.Detail = fmt.Sprintf("plist written but launchctl bootstrap failed: %v: %s",
			err, strings.TrimSpace(string(out)))
		return st, fmt.Errorf("supervise: launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	st.Loaded = true
	st.Detail = "loaded; runs at load and is restarted if it exits"
	return st, nil
}

func PostInstall() (Status, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Status{}, err
	}
	stateDir := filepath.Join(home, ".local", "state", "trajectory-shipper")
	spec, err := common.NewServiceSpec(stateDir, 0, 0)
	if err != nil {
		return Status{}, err
	}
	expected := filepath.Join(home, "Applications", "Shipper.app", "Contents", "MacOS", "shipper")
	if resolved, err := filepath.EvalSymlinks(expected); err == nil {
		expected = resolved
	}
	if spec.Executable != expected {
		return Status{}, fmt.Errorf("postinstall must run from %s, not %s", expected, spec.Executable)
	}
	if err := common.ValidateInstall(spec); err != nil {
		return Status{}, err
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

func ServiceState() Status {
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

	target := guiService()
	out, err := exec.Command(launchctl, "print", target).CombinedOutput()
	if err != nil {
		switch {
		case st.Installed:
			// Written but not loaded is the state that collects nothing while looking installed.
			st.Detail = "plist present but NOT loaded: re-run the Shipper installer"
		default:
			st.Detail = "no agent installed; `shipper run` works in the foreground"
		}
		return st
	}
	st.Loaded = true
	st.Detail = summariseLaunchctlPrint(string(out))
	return st
}

func RestartService() error {
	out, err := exec.Command(launchctl, "kickstart", "-k", guiService()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func RestartCommand() string {
	return "launchctl kickstart -k gui/$(id -u)/" + common.Label
}

// summariseLaunchctlPrint pulls whether a process is running and how it last exited.
func summariseLaunchctlPrint(out string) string {
	var pid, lastExit string
	for _, line := range strings.Split(out, "\n") {
		f := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(f, "pid = "):
			pid = strings.TrimPrefix(f, "pid = ")
		case strings.HasPrefix(f, "last exit code = "):
			lastExit = strings.TrimPrefix(f, "last exit code = ")
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
