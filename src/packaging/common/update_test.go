package common

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestUpdateReExec(t *testing.T) {
	root := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	build := func(name, source string) string {
		t.Helper()
		path := filepath.Join(root, name+".go")
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(root, name+suffix)
		if out, err := exec.Command("go", "build", "-o", binary, path).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v, %s", name, err, out)
		}
		return binary
	}
	updateImport, target, apply := "", "common.BinaryTarget", "common.ApplyBinary(raw)"
	if runtime.GOOS == "darwin" {
		updateImport = `"github.com/QuesmaOrg/quesma-shipper/packaging/macos"`
		target, apply = "macos.UpdateTarget", `macos.ApplyTarget(raw, "1.0.1")`
	}
	updater := build("updater", fmt.Sprintf(`package main
import (
	"os"
	"runtime"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	%s
)
func main() {
	release := common.Release{Targets: map[string]string{runtime.GOOS + "/" + runtime.GOARCH: "raw-binary", "darwin/pkg": "package"}}
	if got := %s(release); got != "raw-binary" { panic("unexpected update target: " + got) }
	raw, err := os.ReadFile(os.Args[1])
	if err != nil { panic(err) }
	if err := %s; err != nil { panic(err) }
	if err := common.ReExec(); err != nil { panic(err) }
}
`, updateImport, target, apply))
	replacement := build("replacement", `package main
import ("fmt"; "os"; "strings")
func main() { fmt.Println(strings.Join(os.Args[1:], "|"), os.Getenv("QUESMA_TEST_REEXEC")) }
`)
	updaterRaw, err := os.ReadFile(updater)
	if err != nil {
		t.Fatal(err)
	}
	replacementRaw, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "QUESMA_TEST_REEXEC=preserved", SupervisedEnv+"=")
	for _, name := range []string{"direct", "symlink"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			installed := filepath.Join(dir, "Caskroom", "quesma-shipper", "1.0.0", "quesma-shipper"+suffix)
			if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installed, updaterRaw, 0o755); err != nil {
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
			plist := filepath.Join(dir, "agent.plist")
			if runtime.GOOS == "darwin" {
				var escaped bytes.Buffer
				if err := xml.EscapeText(&escaped, []byte(installed)); err != nil {
					t.Fatal(err)
				}
				service := "<plist><dict><key>ProgramArguments</key><array><string>" + escaped.String() + "</string></array></dict></plist>"
				if err := os.WriteFile(plist, []byte(service), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(launch, replacement, "updated")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != replacement+"|updated preserved\n" {
				t.Fatalf("update and reexec: %v, %s", err, out)
			}
			updated, err := os.ReadFile(installed)
			if err != nil || !bytes.Equal(updated, replacementRaw) {
				t.Fatalf("installed binary differs from replacement: %v", err)
			}
			if name == "symlink" {
				if target, err := os.Readlink(launch); err != nil || target != installed {
					t.Fatalf("command link changed: %q, %v", target, err)
				}
			}
			if runtime.GOOS == "darwin" && ServiceProgram(Status{Path: plist}) != installed {
				t.Fatal("service program changed")
			}
			cmd = exec.Command(launch, "next launch")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != "next launch preserved\n" {
				t.Fatalf("launch after update: %v, %s", err, out)
			}
		})
	}
}
