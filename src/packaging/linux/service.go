//go:build linux

package linux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type Spec = common.Spec
type Status = common.Status

// unitName is the systemd --user unit. A service, not a timer: the loop owns its own ticker,
// and a fresh process per tick would contend for the flock and drop the backoff state.
const unitName = "trajectory-shipper.service"

func systemdPath(home string) string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "systemd", "user", unitName)
}

// hasSystemdUser reports whether this host has a systemd user instance; containers often do not.
func Available() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	// `systemctl --user` needs a running user instance; a container or session-less login has none.
	if err := exec.Command("systemctl", "--user", "is-system-running").Run(); err != nil {
		out, _ := exec.Command("systemctl", "--user", "show-environment").CombinedOutput()
		if strings.Contains(string(out), "Failed to connect to bus") || len(out) == 0 {
			return false
		}
	}
	return true
}

// renderUnit builds the service unit: Restart=always so a death is not a stop, and
// WantedBy=default.target so it starts on login.
func renderUnit(spec Spec) string {
	// Quoted per argument: systemd splits ExecStart on whitespace, so a path with a space would
	// become two arguments. The escaping it wants is C-style inside double quotes.
	parts := make([]string, 0, len(spec.Args)+1)
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		parts = append(parts, systemdQuote(a))
	}
	cmd := strings.Join(parts, " ")

	stop := common.ExitTimeout(spec)

	var env strings.Builder
	if spec.Home != "" {
		fmt.Fprintf(&env, "Environment=HOME=%s\n", spec.Home)
	}
	if spec.StateDir != "" {
		fmt.Fprintf(&env, "Environment=XDG_STATE_HOME=%s\n", filepath.Dir(spec.StateDir))
	}

	return fmt.Sprintf(`[Unit]
Description=Collect local AI-agent trajectories and ship them encrypted
Documentation=https://github.com/QuesmaOrg/quesma-shipper
After=network-online.target

[Service]
Type=simple
ExecStart=%s
%sRestart=always
RestartSec=30
# How long systemd waits after SIGTERM before killing. Its default is 90s, so the number has
# to be the client's drain deadline, not the init system's.
TimeoutStopSec=%d
# A per-user service, installed with systemctl --user. It never names a user to run as, and is
# never installed system-wide: everything this reads is user-owned, and running it as root
# would resolve ~ to the wrong home.
Nice=10
# journald keeps stdout/stderr. Supervision that discards output makes "it silently never
# runs" undiagnosable.
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
`, cmd, env.String(), int(stop.Seconds()))
}

func InstallService(spec Spec) (Status, error) {
	home, err := common.HomeFor(spec)
	if err != nil {
		return Status{}, err
	}
	path := systemdPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Status{}, fmt.Errorf("supervise: %w", err)
	}
	// Atomic: systemd re-reads units on daemon-reload, and a torn unit fails to parse.
	if err := platform.WriteAtomic(path, []byte(renderUnit(spec)), common.EntryMode); err != nil {
		return Status{}, fmt.Errorf("supervise: write %s: %w", path, err)
	}

	st := Status{Kind: common.KindSystemd, Installed: true, Path: path}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		st.Detail = fmt.Sprintf("unit written but daemon-reload failed: %v: %s", err, strings.TrimSpace(string(out)))
		return st, fmt.Errorf("supervise: systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// enable then restart, not `enable --now`: start is a no-op and would keep the old binary.
	for _, verb := range []string{"enable", "restart"} {
		if out, err := exec.Command("systemctl", "--user", verb, unitName).CombinedOutput(); err != nil {
			st.Detail = fmt.Sprintf("unit written but %s failed: %v: %s", verb, err, strings.TrimSpace(string(out)))
			return st, fmt.Errorf("supervise: systemctl --user %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
		}
	}
	st.Loaded = true
	st.Detail = "enabled and started"
	if hint := lingerHint(); hint != "" {
		st.Detail += "; " + hint
	}
	return st, nil
}

// lingerHint is the session-less-box caveat: without linger a --user service stops with the
// last session and never starts at boot. A hint, not enabled here; linger is the owner's call.
func lingerHint() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	out, err := exec.Command("loginctl", "show-user", user, "--property=Linger").Output()
	if err == nil && strings.Contains(string(out), "Linger=yes") {
		return ""
	}
	return fmt.Sprintf("linger is off, so this stops when your session ends and will not start "+
		"at boot — run `sudo loginctl enable-linger %s` on a session-less host", user)
}

func UninstallService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Stop and disable BEFORE removing the file, or systemd holds a unit it cannot describe.
	if out, err := exec.Command("systemctl", "--user", "disable", "--now", unitName).CombinedOutput(); err != nil {
		trimmed := strings.TrimSpace(string(out))
		if !strings.Contains(trimmed, "not loaded") && !strings.Contains(trimmed, "does not exist") {
			return fmt.Errorf("supervise: systemctl --user disable --now: %w: %s", err, trimmed)
		}
	}
	if err := os.Remove(systemdPath(home)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("supervise: remove unit: %w", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func ServiceState() Status {
	st := Status{Kind: common.KindSystemd}
	home, err := os.UserHomeDir()
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	st.Path = systemdPath(home)
	if _, err := os.Stat(st.Path); err == nil {
		st.Installed = true
	}

	active, _ := exec.Command("systemctl", "--user", "is-active", unitName).Output()
	enabled, _ := exec.Command("systemctl", "--user", "is-enabled", unitName).Output()
	activeState := strings.TrimSpace(string(active))
	enabledState := strings.TrimSpace(string(enabled))

	switch {
	case activeState == "active":
		st.Loaded = true
		st.Detail = "active and " + enabledState
		if enabledState != "enabled" {
			// Running now, but nothing will start it again: healthy-looking until a reboot.
			st.Detail = "active but NOT enabled: it will not start at login"
		}
	case st.Installed:
		st.Detail = fmt.Sprintf("unit present but %s (%s): re-run the Shipper installer",
			orUnknown(activeState), orUnknown(enabledState))
	default:
		st.Detail = "no agent installed; `shipper run` works in the foreground"
	}
	if hint := lingerHint(); hint != "" && st.Loaded {
		st.Detail += "; " + hint
	}
	return st
}

func RestartService() error {
	out, err := exec.Command("systemctl", "--user", "restart", unitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func RestartCommand() string {
	return "systemctl --user restart " + unitName
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// systemdQuote renders one ExecStart argument, bare when plainly safe. The risk is the percent
// sign: systemd expands specifiers in unit files, so a path containing one has to double it.
func systemdQuote(a string) string {
	safe := a != ""
	for _, r := range a {
		if !(r == '/' || r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			safe = false
			break
		}
	}
	if safe {
		return a
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(a) + `"`
}
