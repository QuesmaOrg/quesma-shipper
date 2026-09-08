package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

const PauseUsage = "pause takes `15m`, `1h`, `6h`, `12h`, `24h` or `tomorrow`"

type PauseChoice struct {
	Label string
	Arg   string
	Until func(now time.Time) time.Time
}

var PauseChoices = []PauseChoice{
	{"15 min", "15m", after(15 * time.Minute)},
	{"1 h", "1h", after(time.Hour)},
	{"6 h", "6h", after(6 * time.Hour)},
	{"12 h", "12h", after(12 * time.Hour)},
	{"24 h", "24h", after(24 * time.Hour)},
	{"until tomorrow 9:00", "tomorrow", tomorrowAt(9, 0)},
}

func after(d time.Duration) func(time.Time) time.Time {
	return func(now time.Time) time.Time { return now.Add(d) }
}

func tomorrowAt(h, m int) func(time.Time) time.Time {
	return func(now time.Time) time.Time {
		t := now.AddDate(0, 0, 1)
		return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, now.Location())
	}
}

func ParsePauseUntil(args []string, now time.Time) (time.Time, error) {
	if len(args) == 1 {
		for _, c := range PauseChoices {
			if c.Arg == args[0] {
				return c.Until(now), nil
			}
		}
	}
	return time.Time{}, errors.New(PauseUsage)
}

// pauseStateDir does not require valid config because pause and resume must remain usable
// when configuration is broken.
func pauseStateDir() (dir, warning string, err error) {
	_, paths, resolveErr := ResolveEffective()
	if resolveErr == nil {
		return paths.StateDir, "", nil
	}

	env, err := sources.OSEnv()
	if err != nil {
		return "", "", err
	}
	fallback := config.DefaultPaths(env.Home, env.Lookup).StateDir

	return fallback, fmt.Sprintf("the configuration does not resolve (%v)\n"+
		"using the default state directory %s\n"+
		"if state_dir was moved in config, fix the config and re-run this",
		resolveErr, fallback), nil
}

// StateDirWithoutConfig resolves the state directory the way pause does. The diagnostics record
// needs it for the same reason: a config too broken to load must not also hide the record of what
// broke.
func StateDirWithoutConfig() (string, error) {
	dir, _, err := pauseStateDir()
	return dir, err
}

func Pause(until time.Time) (string, error) {
	stateDir, warning, err := pauseStateDir()
	if err != nil {
		return "", err
	}
	return warning, platform.Set(stateDir, "", time.Now(), until)
}

func Resume() (wasPaused bool, warning string, err error) {
	stateDir, warning, err := pauseStateDir()
	if err != nil {
		return false, "", err
	}
	was := platform.Read(stateDir).Paused
	return was, warning, platform.Clear(stateDir)
}

func FormatUntil(t, now time.Time) string {
	if t.IsZero() {
		return "resumed"
	}
	t = t.In(now.Location())
	day := func(x time.Time) string { return x.Format("2006-01-02") }
	switch day(t) {
	case day(now):
		return t.Format("15:04") + " today"
	case day(now.AddDate(0, 0, 1)):
		return t.Format("15:04") + " tomorrow"
	}
	return t.Format("Mon 2 Jan 15:04")
}
