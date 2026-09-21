package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/codexfake"
)

func TestMain(m *testing.M) {
	codexfake.Main()
	os.Exit(m.Run())
}

// installFakeCodex puts a copy of this test binary on a private PATH as codex and returns the
// environment lookup that makes the collector find it.
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

const codexSignedIn = `{"result":{"account":{"type":"chatgpt","email":"dev@example.org","planType":"pro"},"requiresOpenaiAuth":true}}`

func codexRecords(t *testing.T, raw []byte) []accountObservation {
	t.Helper()
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) != 4 || len(lines[3]) != 0 {
		t.Fatalf("expected three newline-terminated records: %s", raw)
	}
	var out []accountObservation
	for _, line := range lines[:3] {
		var record accountObservation
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		out = append(out, record)
	}
	return out
}

func TestCodexAppServerSnapshotPreservesProviderJSONAndNoToken(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{"tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh","id_token":"fixture-id"}}`)
	req.Env.Lookup = installFakeCodex(t, codexfake.Config{ExpectHome: req.Source.Root, Responses: map[string]json.RawMessage{
		"account/read":            json.RawMessage(codexSignedIn),
		"account/rateLimits/read": json.RawMessage(`{"result":{"rateLimits":{"primary":{"usedPercent":123.456,"resetsAt":9007199254740993,"unknown":null}},"future":[]}}`),
		"account/usage/read":      json.RawMessage(`{"result":{"summary":{"lifetimeTokens":1}}}`),
	}})
	p := Accounts{}
	d, err := p.Discover(req)
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].RelPath != "codex.account.20260916T141500Z.jsonl" {
		t.Fatalf("%+v %v", d, err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Warning != "" {
		t.Fatal(payload.Warning)
	}
	records := codexRecords(t, payload.Bytes)
	for i, source := range []string{"codex.appserver.account", "codex.appserver.rateLimits", "codex.appserver.usage"} {
		if records[i].Source != source || records[i].Error != "" || len(records[i].Body) == 0 {
			t.Fatalf("record %d: %+v", i, records[i])
		}
	}
	raw := payload.Bytes
	for _, field := range []string{`"planType":"pro"`, `"usedPercent":123.456`, `"resetsAt":9007199254740993`, `"unknown":null`, `"future":[]`, `"lifetimeTokens":1`} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("lost %s: %s", field, raw)
		}
	}
	if bytes.Contains(raw, []byte("fixture")) || bytes.Contains(raw, []byte("remoteControl")) || bytes.Contains(raw, []byte("requestApproval")) {
		t.Fatalf("payload carries token or skipped messages: %s", raw)
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("account collection wrote local state")
	}
}

func TestCodexSignedOutSkipsBackendReads(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = installFakeCodex(t, codexfake.Config{Responses: map[string]json.RawMessage{
		"account/read": json.RawMessage(`{"result":{"account":null,"requiresOpenaiAuth":true}}`),
	}})
	d, err := (&Accounts{}).Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	records := codexRecords(t, payload.Bytes)
	if records[0].Error != "" || !bytes.Contains(records[0].Body, []byte(`"account":null`)) {
		t.Fatalf("account: %+v", records[0])
	}
	for _, r := range records[1:] {
		if r.Error != "credentials_unavailable" || r.Body != nil {
			t.Fatalf("signed out must not query the backend: %+v", r)
		}
	}
}

func TestCodexFailuresAreObservationsNotUploads(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  codexfake.Config
		want [3]string
	}{
		{"rpc error", codexfake.Config{Responses: map[string]json.RawMessage{
			"account/read":       json.RawMessage(codexSignedIn),
			"account/usage/read": json.RawMessage(`{"result":{}}`),
		}}, [3]string{"", "rpc_error", ""}},
		{"wrong home", codexfake.Config{ExpectHome: "/nowhere"}, [3]string{"request_failed", "request_failed", "request_failed"}},
		{"oversized", codexfake.Config{Pad: accountResponseLimit}, [3]string{"response_too_large", "rpc_error", "rpc_error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := accountFixture(t)
			accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
			req.Env.Lookup = installFakeCodex(t, tc.cfg)
			d, err := (&Accounts{}).Discover(req)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := d.Candidates[0].Load(req.Context)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(payload.Warning, "partial account snapshot: codex.appserver.") {
				t.Fatalf("warning %q", payload.Warning)
			}
			for i, r := range codexRecords(t, payload.Bytes) {
				if r.Error != tc.want[i] || (r.Error != "") == (len(r.Body) > 0) {
					t.Fatalf("record %d: %+v", i, r)
				}
			}
		})
	}
}

func TestCodexMissingBinaryIsAnObservation(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = func(k string) (string, bool) { return t.TempDir(), k == "PATH" }
	// The developer machine may have a real codex under /opt/homebrew/bin; the fallback dirs are the test's, not the host's.
	installDirs := codexInstallDirs
	codexInstallDirs = []string{t.TempDir()}
	t.Cleanup(func() { codexInstallDirs = installDirs })
	d, err := (&Accounts{}).Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range codexRecords(t, payload.Bytes) {
		if r.Error != "codex_unavailable" || r.Body != nil {
			t.Fatalf("%+v", r)
		}
	}
}

func TestCodexHungAppServerIsKilledAtTheDeadline(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = installFakeCodex(t, codexfake.Config{Hang: true})
	d, err := (&Accounts{}).Discover(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(req.Context, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = d.Candidates[0].Load(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*codexExitGrace+time.Second {
		t.Fatalf("hung child held the flush for %s", elapsed)
	}
}

func TestAccountDiscoveryDoesNotSpawnOrWrite(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = installFakeCodex(t, codexfake.Config{Hang: true})
	for _, capture := range []bool{false, true} {
		req.Capture = capture
		start := time.Now()
		d, err := (&Accounts{}).Discover(req)
		if err != nil || (len(d.Candidates) == 1) != capture {
			t.Fatalf("capture=%v discovery: %+v %v", capture, d, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("discovery spawned the app-server")
		}
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("discovery wrote state")
	}
}
