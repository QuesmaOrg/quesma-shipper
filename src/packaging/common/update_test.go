package common

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--test-update":
			raw, err := os.ReadFile(os.Args[2])
			if err != nil {
				panic(err)
			}
			if err := ApplyBinary(raw); err != nil {
				panic(err)
			}
			os.Args[1] = "--test-restarted"
			if err := ReExec(); err != nil {
				panic(err)
			}
			panic("re-exec unexpectedly returned")
		case "--test-restarted":
			fmt.Println(strings.Join(os.Args[2:], "|"), os.Getenv("QUESMA_TEST_REEXEC"))
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestUpdateReExec(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUESMA_TEST_REEXEC", "preserved")
	t.Setenv(SupervisedEnv, "")
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"direct", "symlink"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			installed := filepath.Join(dir, "quesma-shipper"+suffix)
			if err := os.WriteFile(installed, raw, 0o755); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(installed)
			if err != nil {
				t.Fatal(err)
			}
			launch := installed
			if name == "symlink" {
				launch = filepath.Join(dir, "shipper"+suffix)
				if err := os.Symlink(installed, launch); err != nil {
					// Windows reports missing symlink privileges separately from os.ErrPermission.
					if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(1314))) {
						t.Skipf("Windows symlink permission unavailable: %v", err)
					}
					t.Fatal(err)
				}
			}
			if out, err := exec.Command(launch, "--test-update", executable, "argument with spaces").CombinedOutput(); err != nil || string(out) != executable+"|argument with spaces preserved\n" {
				t.Fatalf("update and reexec: %v, %s", err, out)
			}
			after, err := os.Stat(installed)
			if err != nil || os.SameFile(before, after) {
				t.Fatalf("installed executable was not replaced: %v", err)
			}
			updated, err := os.ReadFile(installed)
			if err != nil || !bytes.Equal(updated, raw) {
				t.Fatalf("installed binary differs from update payload: %v", err)
			}
			if name == "symlink" {
				if target, err := os.Readlink(launch); err != nil || target != installed {
					t.Fatalf("command link changed: %q, %v", target, err)
				}
			}
			if out, err := exec.Command(launch, "--test-restarted", "next launch").CombinedOutput(); err != nil || string(out) != "next launch preserved\n" {
				t.Fatalf("launch after update: %v, %s", err, out)
			}
		})
	}
}
