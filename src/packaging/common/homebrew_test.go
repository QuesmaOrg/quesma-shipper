package common

import (
	"runtime"
	"testing"
)

func TestHomebrewCaskRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew casks use Unix paths")
	}
	for _, tc := range []struct{ path, root string }{
		{"/opt/homebrew/Caskroom/quesma-shipper/0.1.0/quesma-shipper", "/opt/homebrew/Caskroom/quesma-shipper"},
		{"/usr/local/Caskroom/quesma-shipper/0.1.0-123.abc/quesma-shipper", "/usr/local/Caskroom/quesma-shipper"},
		{"/Users/me/Brew & tools/Caskroom/quesma-shipper/0.1.0/quesma-shipper", "/Users/me/Brew & tools/Caskroom/quesma-shipper"},
		{"/Users/me/Applications/Quesma Shipper.app/Contents/MacOS/quesma-shipper", ""},
		{"/Users/me/.local/bin/quesma-shipper", ""},
		{"/opt/homebrew/bin/quesma-shipper", ""},
		{"/opt/homebrew/Caskroom/another-app/0.1.0/quesma-shipper", ""},
		{"/opt/homebrew/Caskroom/quesma-shipper/0.1.0/other", ""},
		{"Caskroom/quesma-shipper/0.1.0/quesma-shipper", ""},
	} {
		if got := HomebrewCaskRoot(tc.path); got != tc.root {
			t.Errorf("HomebrewCaskRoot(%q) = %q, want %q", tc.path, got, tc.root)
		}
	}
}
