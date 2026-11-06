package windows

import (
	"strings"
	"testing"
)

func TestTaskQueryPhases(t *testing.T) {
	var p taskQueryPhases
	if got := p.phase(); got != "PowerShell startup" {
		t.Fatalf("initial phase = %q", got)
	}
	for _, tc := range []struct{ input, want string }{
		{"CO", "PowerShell startup"},
		{"M\r\nconnect\n", "connect"},
		{"secret task\nfolder\n", "folder"},
		{strings.Repeat("x", 10000) + "task\n", "folder"},
		{"task\n", "task"},
		{"task with sensitive details\n", "task"},
	} {
		if n, err := p.Write([]byte(tc.input)); err != nil || n != len(tc.input) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if got := p.phase(); got != tc.want {
			t.Fatalf("phase after %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
