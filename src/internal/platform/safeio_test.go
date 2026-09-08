package platform_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadWhole(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	write(t, p, "{\"a\":1}\n{\"b\":2}\n")

	got, info, err := platform.ReadWhole(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Errorf("content: %q", got)
	}
	if info.Size() != int64(len(got)) {
		t.Errorf("stat size %d, read %d bytes", info.Size(), len(got))
	}
}

// A torn final line is data, not an error: it ships byte-exact and nothing here may repair a tail.
func TestReadWholeKeepsATornTailVerbatim(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "torn.jsonl")
	const torn = "{\"a\":1}\n{\"b\":2"
	write(t, p, torn)

	got, _, err := platform.ReadWhole(p, 0)
	if err != nil {
		t.Fatalf("a truncated last line must not be an error: %v", err)
	}
	if string(got) != torn {
		t.Errorf("tail was altered:\n got %q\nwant %q", got, torn)
	}
}

// The core anti-TOCTOU property: a symlink at the final component is refused, however innocuous its target.
func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.jsonl")
	write(t, real, "{}\n")
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	if _, _, err := platform.Open(link); !errors.Is(err, platform.ErrNotRegular) {
		t.Fatalf("a symlinked path must be refused with ErrNotRegular, got %v", err)
	}
	if _, _, err := platform.ReadWhole(link, 0); !errors.Is(err, platform.ErrNotRegular) {
		t.Fatalf("ReadWhole must refuse a symlink too, got %v", err)
	}
}

// The hazard this exists for: a symlink pointing at credentials, refused on the symlink alone.
func TestOpenRefusesSymlinkToSensitiveTarget(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "credentials.json")
	write(t, secret, `{"access_token":"secret"}`)
	link := filepath.Join(dir, "projects", "innocent.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	body, _, err := platform.ReadWhole(link, 0)
	if err == nil {
		t.Fatalf("read through a symlink returned %d bytes; it must be refused", len(body))
	}
	if strings.Contains(string(body), "secret") {
		t.Fatal("credential content was returned through a symlink")
	}
}

func TestOpenRefusesDirectory(t *testing.T) {
	if _, _, err := platform.Open(t.TempDir()); !errors.Is(err, platform.ErrNotRegular) {
		t.Fatalf("a directory must be refused with ErrNotRegular, got %v", err)
	}
}

func TestReadWholeHonoursSizeLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.jsonl")
	write(t, p, strings.Repeat("x", 1024))

	if _, _, err := platform.ReadWhole(p, 512); !errors.Is(err, platform.ErrTooLarge) {
		t.Fatalf("over-limit read must return ErrTooLarge, got %v", err)
	}
	// Exactly at the limit is fine: the extra byte detects overflow, it does not reject the boundary case.
	if _, _, err := platform.ReadWhole(p, 1024); err != nil {
		t.Fatalf("a file exactly at the limit must be accepted: %v", err)
	}
}

func TestWriteAtomicCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	if err := platform.WriteAtomic(p, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := platform.WriteAtomic(p, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content after replace: %q", got)
	}
}

// No temp file may survive a successful write, or every run would leak one alongside each document.
func TestWriteAtomicLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	if err := platform.WriteAtomic(filepath.Join(dir, "x.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file survived a successful write: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly the target file, got %d entries", len(entries))
	}
}

// The name is fixed and the file is the last run's, so opening it must leave nothing of the previous run behind.
func TestOpenTruncatingEmptiesWhatItOpens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "last-sync.log")
	write(t, p, strings.Repeat("previous run\n", 100))

	f, err := platform.OpenTruncating(p, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this run\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "this run\n" {
		t.Errorf("content after reopen: %q", got)
	}
}

// A fixed name in a world-known directory is the classic symlink target.
func TestOpenTruncatingRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "precious.json")
	write(t, target, `{"keep":"me"}`)
	link := filepath.Join(dir, "last-sync.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	f, err := platform.OpenTruncating(link, 0o600)
	if err == nil {
		f.Close()
		t.Fatal("a symlinked path was opened for truncation")
	}
	if !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("want ErrNotRegular, got %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"keep":"me"}` {
		t.Errorf("the symlink's target was truncated: %q", got)
	}
}

// A stranded temp from a crashed run must never block a later write: the temp name is random,
// never derived from the pid, so a recurring pid cannot recreate the name. There is no cleanup,
// by decision: the leftover is inert garbage and stays.
func TestWriteAtomicIsNotWedgedByAStrandedTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	stale := fmt.Sprintf("%s.tmp-%d", p, os.Getpid())
	if err := os.WriteFile(stale, []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := platform.WriteAtomic(p, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("a stranded temp wedged the write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Errorf("content: got %q want %q", got, "fresh")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("the stranded temp should be left alone (no cleanup, by decision): %v", err)
	}
}
