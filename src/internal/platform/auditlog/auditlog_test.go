package auditlog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
)

func open(t *testing.T) (*auditlog.Log, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := auditlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l, filepath.Join(dir, auditlog.FileName)
}

func TestAppendAndTail(t *testing.T) {
	l, path := open(t)

	for _, d := range []auditlog.Decision{
		auditlog.DecisionShipped,
		auditlog.DecisionUnchanged,
		auditlog.DecisionParked,
	} {
		if err := l.Append(auditlog.Entry{
			Decision: d,
			SourceID: "claude-code-transcripts",
			File:     "/Users/__USER__/.claude/projects/p/a.jsonl",
			BytesIn:  4096,
			BytesOut: 1200,
			RuleHits: map[string]int{"github-pat": 1},
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := auditlog.Tail(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Decision != auditlog.DecisionShipped {
		t.Errorf("order: first entry is %q", entries[0].Decision)
	}
	if entries[2].Decision != auditlog.DecisionParked {
		t.Errorf("order: last entry is %q", entries[2].Decision)
	}
	if entries[0].At.IsZero() {
		t.Error("entries must be timestamped")
	}
}

func TestTailLimits(t *testing.T) {
	l, path := open(t)
	for range 10 {
		if err := l.Append(auditlog.Entry{Decision: auditlog.DecisionShipped}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := auditlog.Tail(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
}

// The log is append-only: an entry already on disk is never rewritten, which is what makes it an audit log.
func TestLogIsAppendOnly(t *testing.T) {
	l, path := open(t)

	if err := l.Append(auditlog.Entry{Decision: auditlog.DecisionShipped, File: "first"}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(auditlog.Entry{Decision: auditlog.DecisionShipped, File: "second"}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(second), string(first)) {
		t.Error("an existing entry was rewritten; the log must only grow")
	}
}

// A reason string that quotes payload is withheld entirely rather than trimmed.
func TestReasonCarryingPayloadIsWithheld(t *testing.T) {
	l, path := open(t)

	if err := l.Append(auditlog.Entry{
		Decision: auditlog.DecisionFailed,
		Reason:   `failed on record {"text":"__REDACTED:github-pat__ and more"}`,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := auditlog.Tail(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(entries[0].Reason, "__REDACTED:") {
		t.Errorf("a payload-derived reason must be withheld, got %q", entries[0].Reason)
	}
	if !strings.Contains(entries[0].Reason, "withheld") {
		t.Errorf("the withholding must be visible rather than silent, got %q", entries[0].Reason)
	}
}

// A newline inside a reason would split one record into two, so reasons are flattened.
func TestReasonNewlinesAreFlattened(t *testing.T) {
	l, path := open(t)
	if err := l.Append(auditlog.Entry{
		Decision: auditlog.DecisionFailed,
		Reason:   "line one\nline two\r\nline three",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimRight(string(raw), "\n"), "\n") != 0 {
		t.Error("a multi-line reason produced multiple log lines")
	}
	entries, err := auditlog.Tail(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected one entry, got %d", len(entries))
	}
}

func TestOverlongReasonIsTruncated(t *testing.T) {
	l, path := open(t)
	if err := l.Append(auditlog.Entry{
		Decision: auditlog.DecisionFailed,
		Reason:   strings.Repeat("x", 5000),
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := auditlog.Tail(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries[0].Reason) > 600 {
		t.Errorf("reason not truncated: %d bytes", len(entries[0].Reason))
	}
	if !strings.Contains(entries[0].Reason, "truncated") {
		t.Error("truncation should be visible")
	}
}

// A torn last line from a crash must not make the whole log unreadable.
func TestTornLastLineDoesNotBreakTheRead(t *testing.T) {
	l, path := open(t)
	if err := l.Append(auditlog.Entry{Decision: auditlog.DecisionShipped, File: "good"}); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"decision":"shipped","fi`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	entries, err := auditlog.Tail(path, 0)
	if err != nil {
		t.Fatalf("a torn line must not fail the read: %v", err)
	}
	if len(entries) != 1 || entries[0].File != "good" {
		t.Errorf("the complete entry should survive: %v", entries)
	}
}

func TestTailOnMissingLogIsEmptyNotAnError(t *testing.T) {
	entries, err := auditlog.Tail(filepath.Join(t.TempDir(), "nope.log"), 0)
	if err != nil {
		t.Fatalf("a missing log is not an error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
}

// Entry has no field for a redacted value or for payload content: the discipline is structural.
func TestEntryHasNoContentFields(t *testing.T) {
	l, path := open(t)
	if err := l.Append(auditlog.Entry{
		Decision:         auditlog.DecisionShipped,
		File:             "/Users/__USER__/.claude/projects/p/a.jsonl",
		RedactionDensity: 0.012,
		RuleHits:         map[string]int{"aws-secret-key": 4},
		ObjectKey:        "v1/organization=default/install=x/mirror/source=s/abc.age",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The rule id is recorded; the value it matched cannot be, because there is nowhere to put it.
	for _, forbidden := range []string{"content", "payload", "value", "secret\":"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("log line contains %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), "aws-secret-key") {
		t.Error("the rule id should be recorded: it is what makes scrubbing queryable")
	}
}
