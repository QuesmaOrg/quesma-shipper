package auditlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tailing must cost the answer, not the history: internal, because a whole-file read returns the same entries.
func TestTailingALargeLogReadsOnlyTheEndOfIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

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
	if _, err := Tail(path, 3); err != nil {
		t.Fatal(err)
	}
	read := bytesRead.Load()

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
