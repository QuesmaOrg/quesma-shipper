package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

type homebrewUpdateTransport struct{ calls int }

func (transport *homebrewUpdateTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls++
	return nil, errors.New("test update endpoint unavailable")
}

func TestHomebrewSelfUpdatesButBrewUninstalls(t *testing.T) {
	if os.Getenv("QUESMA_TEST_BREW_CHILD") == "1" {
		if !packaging.HomebrewManaged() {
			t.Fatal("did not recognize the cask executable")
		}
		build := app.Build{Version: "1.0.0", Release: true}
		var out bytes.Buffer
		transport := &homebrewUpdateTransport{}
		previous := http.DefaultTransport
		http.DefaultTransport = transport
		defer func() { http.DefaultTransport = previous }()
		t.Setenv(app.NoSelfUpdateEnv, "1")
		t.Setenv(app.ReexecGuardEnv, "")
		maybeSelfUpdate(context.Background(), build, &out)
		if transport.calls != 0 {
			t.Fatal("self-update ignored the explicit disable switch")
		}
		t.Setenv(app.NoSelfUpdateEnv, "")
		out.Reset()
		maybeSelfUpdate(context.Background(), build, &out)
		if transport.calls == 0 || !strings.Contains(out.String(), "test update endpoint unavailable") {
			t.Fatalf("automatic update did not reach TUF: %s", &out)
		}
		transport.calls = 0
		if _, err := packaging.Update(context.Background(), packaging.UpdateOptions{}); err == nil || transport.calls == 0 {
			t.Fatalf("packaging.Update = %v", err)
		}
		if _, err := app.Uninstall(true, func(app.UninstallStep) { t.Fatal("uninstall started") }); err == nil || !strings.Contains(err.Error(), "brew uninstall") {
			t.Fatalf("app.Uninstall = %v", err)
		}
		transport.calls = 0
		cmd := Root(build, &out, &out)
		cmd.SetArgs([]string{"update"})
		if err := cmd.Execute(); err == nil || transport.calls == 0 || !strings.Contains(err.Error(), "test update endpoint unavailable") {
			t.Fatalf("manual update did not reach TUF: %v", err)
		}
		cmd = Root(build, &out, &out)
		cmd.SetArgs([]string{"uninstall"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "brew uninstall") {
			t.Fatalf("uninstall = %v", err)
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
	cmd := exec.Command(link, "-test.run=^TestHomebrewSelfUpdatesButBrewUninstalls$")
	cmd.Env = append(os.Environ(), "QUESMA_TEST_BREW_CHILD=1", "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cask subprocess: %v\n%s", err, out)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatal("Brew payload was removed:", err)
	}
}
