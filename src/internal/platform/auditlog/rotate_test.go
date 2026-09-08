package auditlog_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
)

// Unrotated, `shipper log tail` stops working past the read cap, on the installs with the most to explain.
func TestTheLogRotatesInsteadOfGrowingForever(t *testing.T) {
	dir := t.TempDir()
	l, err := auditlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Enough entries to pass the threshold; sanitize truncates Reason, so padding reaches disk far smaller.
	pad := strings.Repeat("x", 512)
	for i := 0; i < 40000; i++ {
		if err := l.Append(auditlog.Entry{
			Decision: auditlog.DecisionUnchanged,
			SourceID: "claude-code-transcripts",
			File:     fmt.Sprintf("projects/p/%04d.jsonl", i),
			Reason:   pad,
		}); err != nil {
			t.Fatal(err)
		}
	}

	current := size(t, filepath.Join(dir, auditlog.FileName))
	previous := size(t, filepath.Join(dir, auditlog.FileName+".1"))

	if previous == 0 {
		t.Fatal("nothing rotated; the log grows without bound")
	}
	if current > 16<<20 {
		t.Errorf("the current log is %d bytes, well past the rotation threshold", current)
	}
	// Two generations, no more: the disk cost is bounded and the most recent history survives a rotation.
	if _, err := os.Stat(filepath.Join(dir, auditlog.FileName+".2")); err == nil {
		t.Error("a third generation exists; two is the whole design")
	}
}

// Tailing must work at any size, and must not read the whole file to answer.
func TestTailReadsFromTheEndOfALargeLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditlog.FileName)

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

	entries, err := auditlog.Tail(path, 3)
	if err != nil {
		t.Fatalf("tailing a large log failed: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[2].File != "f19999" {
		t.Errorf("last entry is %q, want the newest line f19999", entries[2].File)
	}
	if entries[0].File != "f19997" {
		t.Errorf("first of the three is %q, want f19997", entries[0].File)
	}
}

// A tail that spans a rotation must still answer, or the rotation creates a blind window exactly when someone looks.
func TestTailReachesIntoThePreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, auditlog.FileName)

	write(t, path+".1", "older-1", "older-2", "older-3")
	write(t, path, "newer-1")

	entries, err := auditlog.Tail(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries across the rotation, got %d", len(entries))
	}
	if entries[len(entries)-1].File != "newer-1" {
		t.Errorf("newest is %q, want newer-1", entries[len(entries)-1].File)
	}
	if entries[0].File != "older-2" {
		t.Errorf("oldest of the three is %q, want older-2", entries[0].File)
	}
}

func write(t *testing.T, path string, files ...string) {
	t.Helper()
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, `{"at":"2026-08-06T00:00:00Z","decision":"unchanged","file":%q}`+"\n", f)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
