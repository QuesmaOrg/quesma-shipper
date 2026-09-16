// The codex account is read over the app-server's sanctioned JSON-RPC (account/read), not by
// decoding the OAuth id_token ourselves: no token ever enters this process. This is the one
// place the collector spawns a subprocess, allowed by name in TestNoExecOutsidePackaging.
package accountprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

// codexProbeTimeout bounds the whole spawn+handshake+read; an app-server hang must not wedge a flush.
const codexProbeTimeout = 15 * time.Second

// codexAccount is the account/read result we keep. raw is the response bytes, hashed for provenance.
type codexAccount struct {
	Type  string
	Email string
	Plan  string
	raw   []byte
}

// resolveCodexBinary finds codex on PATH, then in the locations a login shell has but a LaunchAgent
// or systemd unit usually does not, so the daemon behaves like the terminal that logged in.
func resolveCodexBinary() (string, error) {
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	var dirs []string
	switch runtime.GOOS {
	case "darwin":
		dirs = []string{"/opt/homebrew/bin/codex", "/usr/local/bin/codex"}
	case "linux":
		dirs = []string{"/usr/local/bin/codex", "/usr/bin/codex"}
	}
	for _, d := range dirs {
		if _, err := exec.LookPath(d); err == nil {
			return d, nil
		}
	}
	return "", errors.New("codex not on PATH or in the usual install locations")
}

// readCodexAccount drives command through initialize -> initialized -> account/read and returns the
// account, or (nil, nil) when app-server reports no signed-in account.
func readCodexAccount(ctx context.Context, command string, args ...string) (*codexAccount, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	c := &rpcConn{stdin: stdin, scanner: bufio.NewScanner(stdout)}
	c.scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	if _, err := c.call(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "trajectory-shipper", "version": "1"},
		"capabilities": nil,
	}, nil); err != nil {
		return nil, err
	}
	if err := c.notify("initialized"); err != nil {
		return nil, err
	}
	var resp struct {
		Account *struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	raw, err := c.call(2, "account/read", map[string]any{"refreshToken": false}, &resp)
	if err != nil {
		return nil, err
	}
	if resp.Account == nil {
		return nil, nil
	}
	return &codexAccount{
		Type:  resp.Account.Type,
		Email: resp.Account.Email,
		Plan:  resp.Account.PlanType,
		raw:   raw,
	}, nil
}

// rpcConn is a line-delimited JSON-RPC 2.0 client over the child's stdio.
type rpcConn struct {
	stdin   io.WriteCloser
	scanner *bufio.Scanner
}

func (c *rpcConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *rpcConn) notify(method string) error {
	return c.write(struct {
		Method string `json:"method"`
	}{Method: method})
}

// call sends one request and returns the raw result of the response whose id matches, decoding it
// into out when out is non-nil. Notifications and other ids in between are skipped.
func (c *rpcConn) call(id int, method string, params, out any) (json.RawMessage, error) {
	if err := c.write(struct {
		Method string `json:"method"`
		ID     int    `json:"id"`
		Params any    `json:"params,omitempty"`
	}{Method: method, ID: id, Params: params}); err != nil {
		return nil, err
	}
	for c.scanner.Scan() {
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
			return nil, err
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: app-server error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		if out != nil {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return nil, fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return resp.Result, nil
	}
	if err := c.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New(method + ": app-server exited before responding")
}
