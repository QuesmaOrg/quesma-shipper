package common

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpdateReExec(t *testing.T) {
	if os.Getenv("QUESMA_TEST_REEXEC_CHILD") == "1" {
		raw, err := os.ReadFile(os.Getenv("QUESMA_TEST_REEXEC_REPLACEMENT"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyBinary(raw); err != nil {
			t.Fatal(err)
		}
		os.Args = []string{os.Args[0], "updated"}
		t.Fatal(ReExec())
	}

	root := t.TempDir()
	helper := filepath.Join(root, "replacement.go")
	if err := os.WriteFile(helper, []byte("package main\nimport (\"fmt\"; \"os\")\nfunc main() { fmt.Println(os.Args[1], os.Getenv(\"QUESMA_TEST_REEXEC_CHILD\")) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if out, err := exec.Command("go", "build", "-o", replacement, helper).CombinedOutput(); err != nil {
		t.Fatalf("build replacement: %v, %s", err, out)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, symlink := range []bool{false, true} {
		name := "direct"
		if symlink {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			installed := filepath.Join(t.TempDir(), "quesma-shipper")
			if err := os.WriteFile(installed, raw, 0o755); err != nil {
				t.Fatal(err)
			}
			launch := installed
			if symlink {
				launch = installed + "-link"
				if err := os.Symlink(installed, launch); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(launch, "-test.run=^TestUpdateReExec$")
			cmd.Env = append(os.Environ(), "QUESMA_TEST_REEXEC_CHILD=1", "QUESMA_TEST_REEXEC_REPLACEMENT="+replacement)
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != "updated 1\n" {
				t.Fatalf("update and reexec: %v, %s", err, out)
			}
			updated, err := os.ReadFile(installed)
			if err != nil || bytes.Equal(raw, updated) {
				t.Fatalf("binary was not replaced: %v", err)
			}
		})
	}
}
