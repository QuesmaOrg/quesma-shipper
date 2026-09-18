package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func TestHomebrewLifecycleDefersToBrew(t *testing.T) {
	if os.Getenv("QUESMA_TEST_BREW_CHILD") == "1" {
		if !packaging.HomebrewManaged() {
			t.Fatal("did not recognize the cask executable")
		}
		build := app.Build{Version: "1.0.0", Release: true}
		var out bytes.Buffer
		maybeSelfUpdate(context.Background(), build, &out)
		if out.Len() != 0 {
			t.Fatalf("automatic update ran: %s", &out)
		}
		if _, err := packaging.Update(context.Background(), packaging.UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "brew upgrade") {
			t.Fatalf("packaging.Update = %v", err)
		}
		if _, err := app.Uninstall(true, func(app.UninstallStep) { t.Fatal("uninstall started") }); err == nil || !strings.Contains(err.Error(), "brew uninstall") {
			t.Fatalf("app.Uninstall = %v", err)
		}
		for _, verb := range []string{"update", "uninstall"} {
			cmd := Root(build, &out, &out)
			cmd.SetArgs([]string{verb})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "Homebrew manages") {
				t.Fatalf("%s = %v", verb, err)
			}
		}
		return
	}

	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(root, "Caskroom", "quesma-shipper", "1.0.0", "quesma-shipper")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shipper")
	if err := os.Symlink(installed, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "-test.run=^TestHomebrewLifecycleDefersToBrew$")
	cmd.Env = append(os.Environ(), "QUESMA_TEST_BREW_CHILD=1", "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cask subprocess: %v\n%s", err, out)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatal("Brew payload was removed:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(err) {
		t.Fatalf("lifecycle commands touched local state: %v", err)
	}
}
