package config

import (
	"strings"
	"testing"
	"time"
)

func TestTickIntervalHonoursTheSupportedShapes(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"15m", 15 * time.Minute},     // the documented default, explicitly
		{"5m", 5 * time.Minute},       //
		{"90s", 90 * time.Second},     //
		{"1h", time.Hour},             //
		{"  10m  ", 10 * time.Minute}, // whitespace is not a meaning
	}
	for _, c := range cases {
		got, warn := TickInterval(c.in)
		if got != c.want || warn != "" {
			t.Errorf("TickInterval(%q) = %v, warn=%q; want %v, no warning", c.in, got, warn, c.want)
		}
	}
}

func TestTickIntervalRefusesUnparseableValues(t *testing.T) {
	for _, in := range []string{
		"every day",
		"15",  // bare number: ambiguous, and ParseDuration agrees
		"-5m", // negative
	} {
		_, warn := TickInterval(in)
		if warn == "" || !strings.Contains(warn, in) {
			t.Errorf("TickInterval(%q) must warn naming the rejected value, got %q", in, warn)
		}
	}
}

func TestTickIntervalFloorsHotLoops(t *testing.T) {
	got, warn := TickInterval("5s")
	if got != MinTick || warn == "" {
		t.Errorf("TickInterval(5s) = %v, warn=%q; want the %v floor and a warning", got, warn, MinTick)
	}
}
