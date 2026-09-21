// Package codexfake turns a test binary into a stand-in for `codex app-server`: tests copy the binary
// onto PATH as codex, and TestMain hands control here when the config env var is set. It answers the
// JSON-RPC handshake from a canned response table, so no real Codex install is needed on any OS.
package codexfake

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

const EnvConfig = "QUESMA_SHIPPER_FAKE_CODEX"

type Config struct {
	// Responses maps a method to its response fragment, either {"result":...} or {"error":...}.
	Responses map[string]json.RawMessage `json:"responses"`
	// Hang never answers, so the caller's timeout is what ends the session.
	Hang bool `json:"hang,omitempty"`
	// ExpectHome fails initialize unless the child received this CODEX_HOME.
	ExpectHome string `json:"expect_home,omitempty"`
	// Pad inflates the account/read result by this many bytes; the config travels in an env var, so
	// an oversized response cannot be spelled out there.
	Pad int `json:"pad,omitempty"`
}

// Main serves the fake when the config env var is set and never returns in that case.
func Main() {
	raw, ok := os.LookupEnv(EnvConfig)
	if !ok {
		return
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		os.Exit(3)
	}
	if len(os.Args) != 2 || os.Args[1] != "app-server" {
		os.Exit(4)
	}
	if cfg.Hang {
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
		reply(out, *req.ID, cfg.respond(req.Method, req.Params))
	}
	os.Exit(0)
}

func (cfg Config) respond(method string, params json.RawMessage) json.RawMessage {
	fail := func(msg string) json.RawMessage {
		return json.RawMessage(`{"error":{"code":-32600,"message":` + quote(msg) + `}}`)
	}
	if method == "initialize" && cfg.ExpectHome != "" && os.Getenv("CODEX_HOME") != cfg.ExpectHome {
		return fail("CODEX_HOME=" + os.Getenv("CODEX_HOME"))
	}
	if method == "account/read" {
		var p struct {
			RefreshToken *bool `json:"refreshToken"`
		}
		if json.Unmarshal(params, &p) != nil || p.RefreshToken == nil || *p.RefreshToken {
			return fail("account/read must pass refreshToken:false")
		}
		if cfg.Pad > 0 {
			return json.RawMessage(`{"result":{"account":{"type":"chatgpt"},"pad":"` + strings.Repeat("x", cfg.Pad) + `"}}`)
		}
	}
	if fragment, ok := cfg.Responses[method]; ok {
		return fragment
	}
	if method == "initialize" {
		return json.RawMessage(`{"result":{"userAgent":"codexfake","codexHome":"` + os.Getenv("CODEX_HOME") + `"}}`)
	}
	return json.RawMessage(`{"error":{"code":-32601,"message":"Method not found"}}`)
}

// Before each reply the fake emits a notification and a server-initiated request, the two message
// shapes a client must skip while waiting for its own response.
func reply(out *bufio.Writer, id int, fragment json.RawMessage) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(fragment, &fields); err != nil {
		os.Exit(5)
	}
	fields["jsonrpc"] = json.RawMessage(`"2.0"`)
	fields["id"], _ = json.Marshal(id)
	msg, _ := json.Marshal(fields)
	out.WriteString(`{"jsonrpc":"2.0","method":"remoteControl/status/changed","params":{"status":"disabled"}}` + "\n")
	out.WriteString(`{"jsonrpc":"2.0","id":9999,"method":"item/commandExecution/requestApproval","params":{}}` + "\n")
	out.Write(append(msg, '\n'))
	out.Flush()
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
