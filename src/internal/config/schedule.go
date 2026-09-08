package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultTick is the cadence nobody configured: the design's loss-window bound.
const DefaultTick = 15 * time.Minute

// MinTick floors the cadence: a poll loop at seconds is a hot loop.
const MinTick = time.Minute

// TickInterval turns `mode.schedule` into the tick interval; a bad value is refused with a warning and the default, never approximated.
func TickInterval(schedule string) (time.Duration, string) {
	s := strings.TrimSpace(schedule)
	if s == "" {
		return DefaultTick, ""
	}

	d, err := parseSchedule(s)
	if err != nil {
		return DefaultTick, fmt.Sprintf("mode.schedule %q: %v — ticking every %s instead",
			schedule, err, DefaultTick)
	}
	if d < MinTick {
		return MinTick, fmt.Sprintf("mode.schedule %q is under the %s floor — ticking every %s",
			schedule, MinTick, MinTick)
	}
	return d, ""
}

func parseSchedule(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("not a duration like \"5m\"")
	}
	if d <= 0 {
		return 0, fmt.Errorf("a tick interval must be positive")
	}
	return d, nil
}
