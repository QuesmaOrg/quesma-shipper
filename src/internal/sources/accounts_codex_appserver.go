package sources

// The Codex account is read over `codex app-server`'s JSON-RPC instead of parsing auth.json, so no
// token is decoded or sent by this process. This is the one data-path file that spawns a
// subprocess; TestNoExecOutsidePackaging names it, and the Codex CLI's own desktop app speaks
// the same protocol.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// codexInstallDirs are searched after PATH: a LaunchAgent or systemd unit rarely inherits a login
// shell's PATH. A var so tests can point it away from a real install.
var codexInstallDirs = map[string][]string{
	"darwin": {"/opt/homebrew/bin", "/usr/local/bin"},
	"linux":  {"/usr/local/bin", "/usr/bin"},
}[runtime.GOOS]

// No PATH at all means no search, so a test environment never reaches a real codex.
func codexBinary(env Env) (string, bool) {
	path, ok := env.Lookup("PATH")
	if !ok {
		return "", false
	}
	dirs := append(filepath.SplitList(path), codexInstallDirs...)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if p, err := exec.LookPath(filepath.Join(dir, "codex")); err == nil {
			return p, true
		}
	}
	return "", false
}

const (
	codexAppServerTimeout = 15 * time.Second
	codexExitGrace        = 2 * time.Second
)

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *codexRPCError) Error() string {
	return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message)
}

type codexRPC struct {
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	next    int
	broken  error
}

// runCodexAppServer drives one app-server session: spawn, initialize, the calls fn makes, then EOF on
// stdin ends the child. The timeout bounds everything so a wedged app-server cannot stall a flush.
func runCodexAppServer(ctx context.Context, binary, codexHome string, fn func(*codexRPC) error) error {
	ctx, cancel := context.WithTimeout(ctx, codexAppServerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	cmd.WaitDelay = codexExitGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c := &codexRPC{stdin: stdin, scanner: bufio.NewScanner(stdout)}
	c.scanner.Buffer(make([]byte, 0, 64<<10), accountResponseLimit+64<<10)
	err = c.session(fn)
	_ = stdin.Close()
	exit := time.AfterFunc(codexExitGrace, cancel)
	_ = cmd.Wait()
	exit.Stop()
	return err
}

func (c *codexRPC) session(fn func(*codexRPC) error) error {
	if _, err := c.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "quesma-shipper", "version": "1"},
		"capabilities": nil,
	}); err != nil {
		return err
	}
	if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return err
	}
	return fn(c)
}

func (c *codexRPC) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// call returns the raw result of the response whose id matches. Notifications and server-initiated
// requests, which carry a method, are skipped. A transport failure breaks the session for good.
func (c *codexRPC) call(method string, params any) (json.RawMessage, error) {
	if c.broken != nil {
		return nil, c.broken
	}
	c.next++
	id := c.next
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.broken = err
		return nil, err
	}
	for c.scanner.Scan() {
		var resp struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *codexRPCError  `json:"error"`
		}
		if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
			c.broken = fmt.Errorf("%s: %w", method, err)
			return nil, c.broken
		}
		if resp.Method != "" || resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
	if err := c.scanner.Err(); err != nil {
		c.broken = err
	} else {
		c.broken = errors.New(method + ": app-server exited before responding")
	}
	return nil, c.broken
}
