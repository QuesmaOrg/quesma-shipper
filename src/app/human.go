package app

import (
	"fmt"
	"strings"
	"time"
)

func HumanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return CountNoun(int(d.Hours()), "hour") + " ago"
	case d < 60*24*time.Hour:
		return CountNoun(int(d.Hours())/24, "day") + " ago"
	default:
		return CountNoun(int(d.Hours())/24/30, "month") + " ago"
	}
}

func CountNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return HumanCount(n) + " " + Plural(n, noun)
}

func Plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func HumanCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteString(",")
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
