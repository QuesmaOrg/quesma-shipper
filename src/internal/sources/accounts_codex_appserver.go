package sources

// The Codex account is read over `codex app-server`'s JSON-RPC instead of parsing auth.json, so no
// token is decoded or sent by this process. This is the one data-path file that spawns a
// subprocess; TestNoExecOutsidePackaging names it.

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

// A LaunchAgent or systemd unit rarely inherits a login shell's PATH, so these follow it.
var codexInstallDirs = map[string][]string{
	"darwin": {"/opt/homebrew/bin", "/usr/local/bin"},
	"linux":  {"/usr/local/bin", "/usr/bin"},
}[runtime.GOOS]

// Every service manager sets PATH; an environment without one is a test, and gets no search.
func codexBinary(env Env) (string, bool) {
	path, ok := env.lookup("PATH")
	if !ok {
		return "", false
	}
	for _, dir := range append(filepath.SplitList(path), codexInstallDirs...) {
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
}

// runCodexAppServer spawns the child, runs the handshake, then fn's calls. EOF on stdin asks the
// child to exit; the grace timer cancels the context if it does not, and WaitDelay bounds the wait.
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
	defer func() {
		_ = stdin.Close()
		exit := time.AfterFunc(codexExitGrace, cancel)
		_ = cmd.Wait()
		exit.Stop()
	}()
	c := &codexRPC{stdin: stdin, scanner: bufio.NewScanner(stdout)}
	c.scanner.Buffer(make([]byte, 0, 64<<10), accountResponseLimit+64<<10)
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

// call returns the result of the response whose id matches; notifications and server-initiated
// requests, which carry a method, are skipped.
func (c *codexRPC) call(method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
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
			return nil, fmt.Errorf("%s: %w", method, err)
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
		return nil, err
	}
	return nil, errors.New(method + ": app-server exited before responding")
}
