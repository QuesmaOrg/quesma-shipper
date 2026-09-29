package auditlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tailing must work at any size and cost the answer, not the history: internal, because a whole-file read returns the same entries.
func TestTailingALargeLogReadsOnlyTheEndOfIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	// 40 MiB, well past anything worth loading to answer "what were the last three things".
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("y", 2<<10)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(f, `{"at":"2026-08-06T00:00:00Z","decision":"unchanged","file":"f%05d","reason":"%s"}`+"\n", i, pad)
	}
	f.Close()

	size := mustSize(t, path)
	if size < 30<<20 {
		t.Fatalf("the fixture is only %d bytes; it cannot show the difference", size)
	}

	bytesRead.Store(0)
	entries, err := Tail(path, 3)
	if err != nil {
		t.Fatalf("tailing a large log failed: %v", err)
	}
	read := bytesRead.Load()

	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[2].File != "f19999" {
		t.Errorf("last entry is %q, want the newest line f19999", entries[2].File)
	}
	if entries[0].File != "f19997" {
		t.Errorf("first of the three is %q, want f19997", entries[0].File)
	}

	if read > 1<<20 {
		t.Errorf("tailing 3 lines from a %d byte log read %d bytes; it should read the end, "+
			"not the file", size, read)
	}
	if read == 0 {
		t.Error("nothing was read; the counter is not wired and this test proves nothing")
	}
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
