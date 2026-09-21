package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/codexfake"
)

func TestMain(m *testing.M) {
	codexfake.Main()
	os.Exit(m.Run())
}

// installFakeCodex puts a copy of this test binary on a private PATH as codex; the returned lookup
// is what the Codex account collector resolves the binary through.
func installFakeCodex(t *testing.T, cfg codexfake.Config) func(string) (string, bool) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), bin, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexfake.EnvConfig, string(raw))
	return func(k string) (string, bool) { return dir, k == "PATH" }
}
