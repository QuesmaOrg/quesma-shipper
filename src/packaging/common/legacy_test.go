package common

import (
	"path/filepath"
	"testing"
)

func TestRenamedExecutableOnlyTouchesTheFormerName(t *testing.T) {
	if got, ok := RenamedExecutable("/home/jane/.local/bin/shipper"); !ok || got != filepath.Clean("/home/jane/.local/bin/quesma-shipper") {
		t.Fatalf("RenamedExecutable() = %q, %v", got, ok)
	}
	for _, exe := range []string{"/home/jane/.local/bin/quesma-shipper", "/opt/shipper/bin/agent"} {
		if _, ok := RenamedExecutable(exe); ok {
			t.Errorf("%s was treated as the former binary", exe)
		}
	}
}
