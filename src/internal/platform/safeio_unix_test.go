//go:build unix

package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// The mode is explicit rather than left to umask: the identity unit must be 0600 whatever the ambient umask is.
func TestWriteAtomicSetsModeRegardlessOfUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	p := filepath.Join(t.TempDir(), "secret.json")
	if err := platform.WriteAtomic(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %#o, want 0600", perm)
	}
}

// The run log lives in the state directory, which must stay private whatever umask the operator's shell carries.
func TestOpenTruncatingSetsModeRegardlessOfUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	p := filepath.Join(t.TempDir(), "last-sync.log")
	f, err := platform.OpenTruncating(p, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %#o, want 0600", perm)
	}
}

// A fifo with no reader HANGS a blocking open from inside the section that holds the store lock, so the refusal must come from the open.
func TestOpenTruncatingRefusesFifo(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "last-sync.log")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("cannot create fifos here: %v", err)
	}

	var f *os.File
	var err error
	done := make(chan struct{})
	go func() { f, err = platform.OpenTruncating(pipe, 0o600); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OpenTruncating blocked on a readerless fifo instead of refusing it")
	}
	if err == nil {
		f.Close()
		t.Fatal("a fifo was opened for truncation")
	}
	if !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("want ErrNotRegular, got %v", err)
	}
}

// With a reader attached the open succeeds, so this refusal comes from the fstat check rather than ENXIO.
func TestOpenTruncatingRefusesFifoWithReader(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "last-sync.log")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("cannot create fifos here: %v", err)
	}
	r, err := os.OpenFile(pipe, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	f, err := platform.OpenTruncating(pipe, 0o600)
	if err == nil {
		f.Close()
		t.Fatal("a fifo with a reader was opened for truncation")
	}
	if !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("want ErrNotRegular, got %v", err)
	}
}

// A partial write must never be visible under the target name: the rename is the commit point.
func TestWriteAtomicNeverExposesPartialContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := platform.WriteAtomic(p, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A read-only directory fails the write before any rename can land.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	if err := platform.WriteAtomic(p, []byte("newer"), 0o600); err == nil {
		t.Fatal("an unwritable directory should surface as an error, not a silent overwrite")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "good" {
		t.Errorf("the previous content must survive a failed write, got %q", got)
	}
}
