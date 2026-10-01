package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform/crashjournal"
)

func TestRecycleDoesNotLeaveACrash(t *testing.T) {
	const stageEnv = "QUESMA_TEST_RECYCLE_STAGE"
	const dirEnv = "QUESMA_TEST_RECYCLE_DIR"
	switch os.Getenv(stageEnv) {
	case "start":
		fl, err := crashjournal.Open(os.Getenv(dirEnv), "recycled-run")
		if err != nil {
			t.Fatal(err)
		}
		fl.Start()
		fl.Phase("tick 13")
		if err := os.Setenv(stageEnv, "replaced"); err != nil {
			t.Fatal(err)
		}
		t.Fatal(recycleProcess(fl))
	case "replaced":
		fmt.Println("process replaced")
		os.Exit(0)
	}

	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRecycleDoesNotLeaveACrash$")
	cmd.Env = []string{
		stageEnv + "=start", dirEnv + "=" + dir,
		"HOME=" + dir, "USERPROFILE=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
	}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "process replaced\n" {
		t.Fatalf("recycle: %v, %s", err, out)
	}
	// Check after the child dies: its live PID would hide an unclean run.
	if crash := crashjournal.LastRun(dir); crash != nil {
		t.Fatalf("intentional recycle reported as a crash: %+v", crash)
	}
}
