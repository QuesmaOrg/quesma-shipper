package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	const latest = "0.0.1-124.def456def456"

	for _, tc := range []struct {
		name    string
		current string
		want    bool
	}{
		{"older release", "0.0.1-123.abcdef123456", true},
		{"same release", latest, false},
		{"newer release", "0.0.1-125.aaaaaaaaaaaa", false},
		{"numeric commit count", "0.0.1-99.bbb222bbb222", true},
		{"development build", "0.0.0-031a7faa8c16", true},
		{"unknown build", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newer(latest, tc.current); got != tc.want {
				t.Errorf("newer(%q, %q) = %v, want %v", latest, tc.current, got, tc.want)
			}
		})
	}
}

func TestNewReleaseLineSupersedesOldRepository(t *testing.T) {
	const (
		latest  = "0.0.2-1.abcdef123456"
		current = "0.0.1-283.ad5a94045497"
	)
	if !newer(latest, current) {
		t.Fatalf("newer(%q, %q) = false, want true", latest, current)
	}
}

// The child updates its own installed copy and re-execs; only a restart that kept argv and the
// environment can run this test again and print "restarted".
func TestReExecAfterUpdate(t *testing.T) {
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("QUESMA_TEST_REEXEC") {
	case "update":
		if err := ApplyBinary(raw); err != nil {
			t.Fatal(err)
		}
		os.Setenv("QUESMA_TEST_REEXEC", "restarted")
		t.Fatal(ReExec())
	case "restarted":
		fmt.Println("restarted")
		return
	}
	dir := t.TempDir()
	installed := filepath.Join(dir, filepath.Base(os.Args[0]))
	if err := os.WriteFile(installed, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	launches := []string{installed}
	if runtime.GOOS != "windows" { // Homebrew launches through a symlink
		link := filepath.Join(dir, "shipper")
		if err := os.Symlink(installed, link); err != nil {
			t.Fatal(err)
		}
		launches = append(launches, link)
	}
	for _, launch := range launches {
		cmd := exec.Command(launch, "-test.run=^TestReExecAfterUpdate$")
		cmd.Env = append(os.Environ(), "QUESMA_TEST_REEXEC=update")
		if out, err := cmd.CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "restarted\n") {
			t.Fatalf("%s: update and re-exec: %v, %s", launch, err, out)
		}
	}
}
