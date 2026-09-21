package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The fake app-server is this test binary: TestMain copies it onto a private PATH as codex once, and
// a child started with the config env var serves canned responses instead of running tests.
const fakeCodexEnv = "QUESMA_SHIPPER_FAKE_CODEX"

var fakeCodexDir string

type fakeCodex struct {
	Responses  map[string]json.RawMessage `json:"responses"`
	Hang       bool                       `json:"hang,omitempty"`
	ExpectHome string                     `json:"expect_home,omitempty"`
}

func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv(fakeCodexEnv); ok {
		serveFakeCodex(raw)
	}
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		panic(err)
	}
	fakeCodexDir, err = os.MkdirTemp("", "fake-codex")
	if err != nil {
		panic(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(fakeCodexDir, name), bin, 0700); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(fakeCodexDir)
	os.Exit(code)
}

func (f fakeCodex) install(t *testing.T) func(string) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCodexEnv, string(raw))
	return func(k string) (string, bool) { return fakeCodexDir, k == "PATH" }
}

func serveFakeCodex(raw string) {
	var f fakeCodex
	if err := json.Unmarshal([]byte(raw), &f); err != nil || len(os.Args) != 2 || os.Args[1] != "app-server" {
		os.Exit(3)
	}
	if f.Hang {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for in.Scan() {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(in.Bytes(), &req); err != nil || req.ID == nil {
			continue
		}
		// A notification and a server-initiated request precede every reply; clients must skip both.
		fmt.Fprint(out, `{"jsonrpc":"2.0","method":"remoteControl/status/changed","params":{"status":"disabled"}}`+"\n")
		fmt.Fprint(out, `{"jsonrpc":"2.0","id":9999,"method":"item/commandExecution/requestApproval","params":{}}`+"\n")
		fragment := bytes.TrimSpace(f.respond(req.Method, req.Params))
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,%s`+"\n", *req.ID, fragment[1:])
		out.Flush()
	}
	os.Exit(0)
}

func (f fakeCodex) respond(method string, params json.RawMessage) json.RawMessage {
	fail := func(msg string) json.RawMessage {
		quoted, _ := json.Marshal(msg)
		return json.RawMessage(`{"error":{"code":-32600,"message":` + string(quoted) + `}}`)
	}
	if method == "initialize" && f.ExpectHome != "" && os.Getenv("CODEX_HOME") != f.ExpectHome {
		return fail("CODEX_HOME=" + os.Getenv("CODEX_HOME"))
	}
	if method == "account/read" && !bytes.Contains(params, []byte(`"refreshToken":false`)) {
		return fail("account/read must pass refreshToken:false")
	}
	if fragment, ok := f.Responses[method]; ok {
		return fragment
	}
	if method == "initialize" {
		return json.RawMessage(`{"result":{"userAgent":"fake","codexHome":""}}`)
	}
	return json.RawMessage(`{"error":{"code":-32601,"message":"Method not found"}}`)
}

const codexSignedIn = `{"result":{"account":{"type":"chatgpt","email":"dev@example.org","planType":"pro"},"requiresOpenaiAuth":true}}`

// codexSnapshot runs one bucket against the fake and returns the payload and its three records.
func codexSnapshot(t *testing.T, f fakeCodex) (Payload, []accountObservation) {
	t.Helper()
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{"tokens":{"access_token":"fixture-access","id_token":"fixture-id"}}`)
	req.Env.Lookup = f.install(t)
	d, err := (&Accounts{}).Discover(req)
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].RelPath != "codex.account.20260916T141500Z.jsonl" {
		t.Fatalf("%+v %v", d, err)
	}
	payload, err := d.Candidates[0].Load(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(req.StateDir); !os.IsNotExist(err) {
		t.Fatal("account collection wrote local state")
	}
	lines := bytes.Split(payload.Bytes, []byte("\n"))
	if len(lines) != 4 || len(lines[3]) != 0 {
		t.Fatalf("expected three newline-terminated records: %s", payload.Bytes)
	}
	var records []accountObservation
	for _, line := range lines[:3] {
		var record accountObservation
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return payload, records
}

func TestCodexAppServerSnapshotPreservesProviderJSONAndNoToken(t *testing.T) {
	payload, records := codexSnapshot(t, fakeCodex{Responses: map[string]json.RawMessage{
		"account/read":            json.RawMessage(codexSignedIn),
		"account/rateLimits/read": json.RawMessage(`{"result":{"rateLimits":{"primary":{"usedPercent":123.456,"resetsAt":9007199254740993,"unknown":null}},"future":[]}}`),
		"account/usage/read":      json.RawMessage(`{"result":{"summary":{"lifetimeTokens":1}}}`),
	}})
	if payload.Warning != "" {
		t.Fatal(payload.Warning)
	}
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
}

func TestCodexChildReadsTheCollectorRoot(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = fakeCodex{ExpectHome: req.Source.Root, Responses: map[string]json.RawMessage{"account/read": json.RawMessage(codexSignedIn)}}.install(t)
	obs, present := (&Accounts{}).collectCodex(req.Context, req)
	if !present || obs[0].Error != "" {
		t.Fatalf("CODEX_HOME not passed to the child: %+v", obs[0])
	}
}

func TestCodexSignedOutSkipsBackendReads(t *testing.T) {
	_, records := codexSnapshot(t, fakeCodex{Responses: map[string]json.RawMessage{
		"account/read": json.RawMessage(`{"result":{"account":null,"requiresOpenaiAuth":true}}`),
	}})
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
		fake fakeCodex
		want [3]string
	}{
		{"rpc error", fakeCodex{Responses: map[string]json.RawMessage{
			"account/read":       json.RawMessage(codexSignedIn),
			"account/usage/read": json.RawMessage(`{"result":{}}`),
		}}, [3]string{"", "rpc_error", ""}},
		{"handshake refused", fakeCodex{ExpectHome: "/nowhere"}, [3]string{"request_failed", "request_failed", "request_failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, records := codexSnapshot(t, tc.fake)
			if !strings.HasPrefix(payload.Warning, "partial account snapshot: codex.appserver.") {
				t.Fatalf("warning %q", payload.Warning)
			}
			for i, r := range records {
				if r.Error != tc.want[i] || (r.Error != "") == (len(r.Body) > 0) {
					t.Fatalf("record %d: %+v", i, r)
				}
			}
		})
	}
}

func TestCodexResultCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		err  error
		want string
	}{
		{"rpc error", "", &codexRPCError{Code: -32601, Message: "Method not found"}, "rpc_error"},
		{"line too long", "", bufio.ErrTooLong, "response_too_large"},
		{"transport", "", errors.New("broken pipe"), "request_failed"},
		{"oversized", `{"pad":"` + strings.Repeat("x", accountResponseLimit) + `"}`, nil, "response_too_large"},
		{"no result", "", nil, "invalid_json"},
		{"ok", `{"account":null}`, nil, ""},
	} {
		obs := codexResult(accountObservation{Source: "test"}, json.RawMessage(tc.raw), tc.err)
		if obs.Error != tc.want || (tc.want == "") != (len(obs.Body) > 0) {
			t.Fatalf("%s: %+v", tc.name, obs)
		}
	}
}

func TestCodexMissingBinaryIsAnObservation(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	obs, present := (&Accounts{}).collectCodex(req.Context, req)
	if !present || len(obs) != 3 {
		t.Fatalf("%v %+v", present, obs)
	}
	for _, r := range obs {
		if r.Error != "codex_unavailable" || r.Body != nil {
			t.Fatalf("%+v", r)
		}
	}
}

func TestCodexHungAppServerIsKilledAtTheDeadline(t *testing.T) {
	req := accountFixture(t)
	accountFile(t, filepath.Join(req.Source.Root, "auth.json"), `{}`)
	req.Env.Lookup = fakeCodex{Hang: true}.install(t)
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
	req.Env.Lookup = fakeCodex{Hang: true}.install(t)
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
