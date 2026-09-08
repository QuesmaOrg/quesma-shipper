package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A reachable dynamic reflect.Value.MethodByName makes the linker keep every
// exported method of every reachable type; an earlier change paid 9 MB for it via cobra
// help templates. The linker tags such calls <ReflectMethod> in -dumpdep
// (constant-name lookups it can track are not tagged and stay harmless).
func TestNoDynamicMethodByName(t *testing.T) {
	build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "shipper"), "-ldflags=-dumpdep", ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "<ReflectMethod>") {
			t.Errorf("linker dead-code elimination is off, reached via: %s", line)
		}
	}
}
