// Shared service lifecycle for per-user agents. Platform packages own the actual supervisor;
// common owns the input contract and the last-run state used to detect a silent agent.
package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Label is the identifier launchd and systemd know this agent by. Never change it: an installed
// agent would be orphaned, running an old binary on a schedule nothing can see.
const Label = "com.quesma.trajectory-shipper"

// Kind is the supervision mechanism in use on this host.
type Kind string

const (
	KindLaunchd     Kind = "launchd"
	KindSystemd     Kind = "systemd-user"
	KindWindowsTask Kind = "windows-task"

	// KindCron is the non-systemd Linux fallback: the client prints a crontab line, never edits one.
	KindCron Kind = "cron"

	// KindUnsupported means no supervision here; `quesma-shipper run` still works in the foreground.
	KindUnsupported Kind = "unsupported"
)

// Spec is what to install.
type Spec struct {
	// Executable is the absolute path to the binary; a relative path or a moving symlink breaks.
	Executable string

	// Args is the verb the agent runs: `run`, so the flock and the schedule live in one process.
	Args []string

	// Home and StateDir go into the agent's environment; launchd hands an agent almost none.
	Home     string
	StateDir string

	// LogDir is where stdout/stderr go; discarded output makes "it never runs" undiagnosable.
	LogDir string

	// Tick is the configured collection cadence. launchd and systemd ignore it because the loop
	// keeps its own ticker, but the cron fallback IS the ticker. Zero means the 15-minute default.
	Tick time.Duration

	// StopTimeout is how long the supervisor waits after SIGTERM before killing. It comes from
	// configuration because it has to outlast the drain deadline; a killed drain looks clean.
	StopTimeout time.Duration
}

// NewServiceSpec resolves the stable executable behind a package-installed symlink.
func NewServiceSpec(stateDir string, stopTimeout, tick time.Duration) (Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return Spec{}, fmt.Errorf("cannot determine this binary's path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Spec{}, err
	}
	return Spec{Executable: exe, Args: []string{"run"}, Home: home,
		StateDir: stateDir, LogDir: filepath.Join(stateDir, "logs"), StopTimeout: stopTimeout,
		Tick: tick}, nil
}

// EntryMode keeps the per-user service entry private.
const EntryMode = 0o600

// ExitTimeout is the kill window shared by service renderers and shutdown logic.
func ExitTimeout(spec Spec) time.Duration {
	if spec.StopTimeout <= 0 {
		return 5*time.Minute + 30*time.Second
	}
	return spec.StopTimeout + 30*time.Second
}

// Status is what `status` and `doctor` report.
type Status struct {
	Kind Kind

	// Installed means the unit or plist file exists on disk.
	Installed bool

	// Loaded means the supervisor picked it up; written-but-never-loaded is the common failure.
	Loaded bool

	// Path is the unit or plist file.
	Path string

	// Program is set by supervisors whose entries are not represented by a readable file.
	Program string

	// LastRun is when the loop last completed a flush; zero on a loaded agent is the alarm.
	LastRun time.Time

	// Detail explains the state in a sentence, including whatever the supervisor said.
	Detail string
}

// ErrRoot is returned when install is attempted as root.
var ErrRoot = errors.New("supervise: refusing to install as root: this is a per-user agent, " +
	"and running as root would resolve ~ to root's home and read the wrong user's files")

// ValidateInstall checks the invariants shared by every platform service installer.
func ValidateInstall(spec Spec) error {
	if os.Geteuid() == 0 {
		return ErrRoot
	}
	if spec.Executable == "" {
		return errors.New("supervise: no executable path")
	}
	if !filepath.IsAbs(spec.Executable) {
		return fmt.Errorf("supervise: %q is not an absolute path: a relative path in a "+
			"supervision entry breaks as soon as the agent's working directory differs", spec.Executable)
	}
	return nil
}

func HomeFor(spec Spec) (string, error) {
	if spec.Home != "" {
		return spec.Home, nil
	}
	return os.UserHomeDir()
}

// ErrCronManual signals that the caller must print the hint rather than claim an install.
var ErrCronManual = errors.New("supervise: this host has no systemd --user; " +
	"add the printed crontab line yourself")

// RunMarker is the file the loop touches after each completed flush.
const RunMarker = "last_run"

// RecordRun stamps the marker after every flush, including one that shipped nothing: the
// fingerprint document only advances on collection, so it cannot show a quiet install is alive.
func RecordRun(stateDir string, at time.Time) error {
	return platform.WriteAtomic(filepath.Join(stateDir, RunMarker),
		[]byte(at.UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// LastRun reads the marker. Zero time means never.
func LastRun(stateDir string) time.Time {
	raw, _, err := platform.ReadWhole(filepath.Join(stateDir, RunMarker), 128)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// CronHint is the non-systemd fallback, a line the operator adds manually.
func CronHint(spec Spec) string {
	return fmt.Sprintf("%s %s run --once >> %s 2>&1",
		cronExpr(spec.Tick), spec.Executable, filepath.Join(spec.LogDir, "cron.log"))
}

func cronExpr(tick time.Duration) string {
	switch {
	case tick <= 0:
		return "*/15 * * * *"
	case tick <= time.Minute:
		return "* * * * *"
	case tick < time.Hour:
		return fmt.Sprintf("*/%d * * * *", int((tick+time.Minute-1)/time.Minute))
	case tick < 24*time.Hour:
		return fmt.Sprintf("0 */%d * * *", int((tick+time.Hour-1)/time.Hour))
	default:
		return "0 0 * * *"
	}
}

// ServiceProgram reads the executable owned by an installed service entry.
func ServiceProgram(st Status) string {
	if st.Program != "" {
		return st.Program
	}
	raw, err := os.ReadFile(st.Path)
	if err != nil {
		return ""
	}
	text := string(raw)
	if _, after, ok := strings.Cut(text, "<key>ProgramArguments</key>"); ok {
		if _, after, ok = strings.Cut(after, "<string>"); ok {
			program, _, _ := strings.Cut(after, "</string>")
			return program
		}
	}
	if _, after, ok := strings.Cut(text, "\nExecStart="); ok {
		line, _, _ := strings.Cut(after, "\n")
		program := strings.TrimPrefix(strings.Fields(line)[0], "\"")
		return strings.TrimSuffix(program, "\"")
	}
	return ""
}

func RemoveState(stateDir string) error {
	return os.RemoveAll(stateDir)
}
