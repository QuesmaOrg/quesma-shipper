//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO where an object belongs fails that object; opening it would block the run forever.
func TestFIFOObjectFails(t *testing.T) {
	a := newArchive(t)
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)
	fifo := objectKey(installA, claude, "22bb")
	if err := unix.Mkfifo(filepath.Join(a.enc, filepath.FromSlash(fifo)), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() { done <- a.run() }()
	select {
	case code := <-done:
		a.wantSummary(code, 1, 1, 0, 1)
	case <-time.After(30 * time.Second):
		t.Fatal("run blocked on a FIFO")
	}
	a.wantStderr("unseal " + fifo + ": not a regular file")
	a.wantPlain(out(installA, claude, "a/1.jsonl"))
}

// An unreadable tags.json fails the install's objects, retried next run, rather than writing
// them under the install id where the named directory would have been.
func TestUnreadableTagsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	a := newArchive(t)
	a.tags(installA, "laptop")
	key := objectKey(installA, claude, "11aa")
	a.put(key, manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)
	tags := filepath.Join(a.enc, filepath.FromSlash(installRoot(installA)), "tags.json")
	if err := os.Chmod(tags, 0); err != nil {
		t.Fatal(err)
	}

	a.wantSummary(a.run(), 1, 0, 0, 1)
	if want := "unseal " + key + ": " + installRoot(installA) + "/tags.json: "; !strings.HasPrefix(a.stderrStr, want) ||
		!strings.Contains(a.stderrStr, "permission denied") || strings.Count(a.stderrStr, "\n") != 1 {
		t.Fatalf("stderr:\n%s\nwant one line starting %q", a.stderrStr, want)
	}
	a.wantPlain()

	if err := os.Chmod(tags, 0o600); err != nil {
		t.Fatal(err)
	}
	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantPlain(out("laptop (3f2a1b2c)", claude, "a/1.jsonl"))
}
