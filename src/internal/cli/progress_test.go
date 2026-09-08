package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// shipped is one decided file, the only outcome that produces a console line worth budgeting.
func shipped(n int) formats.FileOutcome {
	return formats.FileOutcome{
		SourceID: "claude-code-transcripts",
		RelPath:  fmt.Sprintf("projects/p/%03d.jsonl", n),
		Decision: formats.DecisionShipped,
		BytesIn:  1000,
		BytesOut: 400,
	}
}

// feed decides n files through the stream, as the loop would.
func feed(s *progressStream, n int, f func(int) formats.FileOutcome) {
	for i := 1; i <= n; i++ {
		s.emit("claude-code-transcripts", i, n, f(i))
	}
}

func TestTheConsoleStopsAfterTheBudgetAndSaysWhereTheRestWent(t *testing.T) {
	var console, log bytes.Buffer
	s := newProgressStream(&console, false)
	s.log, s.logPath = &log, "/state/last-sync.log"
	feed(s, consoleLineBudget+10, shipped)

	lines := strings.Split(strings.TrimRight(console.String(), "\n"), "\n")
	// The budget plus the notice: not a terminal, and the plain cadence has not come round.
	if len(lines) != consoleLineBudget+1 {
		t.Fatalf("console printed %d lines, want %d:\n%s",
			len(lines), consoleLineBudget+1, console.String())
	}
	notice := lines[len(lines)-1]
	if !strings.Contains(notice, "/state/last-sync.log") {
		t.Errorf("the notice does not name the log: %q", notice)
	}
	if !strings.Contains(notice, "32 lines shown") {
		t.Errorf("the notice does not say what it cut: %q", notice)
	}
	// Everything is in the log, including the lines the console dropped.
	if got := strings.Count(log.String(), "\n"); got != consoleLineBudget+10 {
		t.Errorf("the log holds %d lines, want %d", got, consoleLineBudget+10)
	}
	if !strings.Contains(log.String(), "042.jsonl") {
		t.Errorf("a line past the console budget never reached the log:\n%s", log.String())
	}
}

// The notice claims lines were cut, so it must not fire on a run that cut nothing.
func TestTheNoticeWaitsForALineTheConsoleActuallyDrops(t *testing.T) {
	var console, log bytes.Buffer
	s := newProgressStream(&console, false)
	s.log, s.logPath = &log, "/state/last-sync.log"
	s.tty = true

	feed(s, consoleLineBudget, shipped)
	if strings.Contains(console.String(), "lines shown") {
		t.Fatalf("a run that showed every line sent the reader to the log:\n%s", console.String())
	}

	s.emit("claude-code-transcripts", 33, 40, formats.FileOutcome{Decision: formats.DecisionUnchanged})
	if strings.Contains(console.String(), "lines shown") {
		t.Errorf("an unchanged file, which prints nothing, triggered the notice:\n%s", console.String())
	}

	s.emit("claude-code-transcripts", 34, 40, shipped(34))
	if !strings.Contains(console.String(), "lines shown") {
		t.Errorf("the first line the console dropped did not say where it went:\n%s", console.String())
	}
}

// A pipe or a CI log gets a heartbeat instead of a bar: \r means nothing there.
func TestANonTerminalGetsAPlainLineEveryHundredFiles(t *testing.T) {
	var console bytes.Buffer
	s := newProgressStream(&console, false)
	feed(s, 250, shipped)

	out := console.String()
	if strings.Contains(out, "\r") {
		t.Errorf("a non-terminal was sent carriage returns:\n%q", out)
	}
	var plain []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sent ") && !strings.HasPrefix(line, "[") {
			plain = append(plain, line)
		}
	}
	// 250 decided files past a 32-line budget: the cadence fires at 100 and 200.
	if len(plain) != 2 {
		t.Fatalf("want two plain progress lines, got %d:\n%s", len(plain), out)
	}
	if !strings.Contains(plain[0], "100/250") {
		t.Errorf("the first plain line is not at the hundredth file: %q", plain[0])
	}
	if strings.Contains(plain[0], "[#") {
		t.Errorf("a non-terminal was drawn a bar: %q", plain[0])
	}
}

// A steady-state run prints nothing, so it must never reach the budget or draw a bar.
func TestUnchangedFilesAreSilentOnBothTheConsoleAndTheLog(t *testing.T) {
	var console, log bytes.Buffer
	s := newProgressStream(&console, false)
	s.log, s.logPath = &log, "/state/last-sync.log"
	s.tty = true
	feed(s, 500, func(int) formats.FileOutcome {
		return formats.FileOutcome{Decision: formats.DecisionUnchanged}
	})
	if console.Len() != 0 {
		t.Errorf("a steady-state run printed %q", console.String())
	}
	if log.Len() != 0 {
		t.Errorf("a steady-state run logged %q", log.String())
	}
}

func TestTheBarRewritesOneLineAndErasesWhatItShortens(t *testing.T) {
	var console bytes.Buffer
	s := newProgressStream(&console, false)
	s.tty = true
	feed(s, consoleLineBudget+3, shipped)

	tail := console.String()[strings.Index(console.String(), "\r"):]
	frames := strings.Split(tail, "\r")[1:]
	if len(frames) != 3 {
		t.Fatalf("want one frame per file past the budget, got %d:\n%q", len(frames), tail)
	}
	if strings.Contains(tail, "\n") {
		t.Errorf("the bar started a new line instead of rewriting its own:\n%q", tail)
	}
	for _, f := range frames {
		if len(f) > barCols {
			t.Errorf("frame is %d columns, over the %d bound: %q", len(f), barCols, f)
		}
	}

	// A shrinking frame must blank the columns the longer one left, or its tail stays on screen.
	s.draw("short")
	drawn := console.String()
	last := drawn[strings.LastIndex(drawn, "\r")+1:]
	if !strings.HasPrefix(last, "short ") || strings.TrimSpace(last) != "short" {
		t.Errorf("a shorter frame did not erase the longer one it replaced: %q", last)
	}
}

func TestFinishTakesTheBarDown(t *testing.T) {
	var console bytes.Buffer
	s := newProgressStream(&console, false)
	s.tty = true
	feed(s, consoleLineBudget+1, shipped)
	before := console.Len()

	s.Finish()
	erased := console.String()[before:]
	if strings.TrimSpace(strings.ReplaceAll(erased, "\r", "")) != "" {
		t.Errorf("Finish wrote something other than blanks: %q", erased)
	}
	if !strings.HasSuffix(erased, "\r") {
		t.Errorf("Finish left the cursor past the erased frame: %q", erased)
	}
	// Idempotent: the summary printer does not have to know whether a bar was ever drawn.
	after := console.Len()
	s.Finish()
	if console.Len() != after {
		t.Errorf("a second Finish wrote %q", console.String()[after:])
	}
}

// A mid-run warning needs the bar down before it and back up after it.
func TestAWarningOverTheBarGetsItsOwnLine(t *testing.T) {
	var console bytes.Buffer
	s := newProgressStream(&console, false)
	s.tty = true
	feed(s, consoleLineBudget+1, shipped)

	fmt.Fprintln(s.Stderr(), "warning: using the cached config")

	out := console.String()
	warn := strings.Index(out, "warning:")
	if warn < 0 {
		t.Fatal("the warning never reached the console")
	}
	before := out[:warn]
	if !strings.HasSuffix(before, "\r") {
		t.Errorf("the warning was written into the bar rather than over it: %q", before[len(before)-40:])
	}
	if !strings.Contains(out[warn:], "sent ") {
		t.Error("the bar was not redrawn after the warning")
	}
}

// --quiet drops console progress and the summary; the log is what an unattended run leaves.
func TestQuietStillWritesTheRunLog(t *testing.T) {
	var console, log bytes.Buffer
	s := newProgressStream(&console, true)
	s.log, s.logPath = &log, "/state/last-sync.log"
	feed(s, 5, shipped)

	if console.Len() != 0 {
		t.Errorf("--quiet printed %q", console.String())
	}
	if got := strings.Count(log.String(), "\n"); got != 5 {
		t.Errorf("the run log holds %d lines, want 5:\n%s", got, log.String())
	}
}

// The e2e harness reads any counter word followed by a number as the run summary, so no
// transient rendering may contain one.
func TestTransientRenderingsCannotBeReadAsARunSummary(t *testing.T) {
	s := newProgressStream(&bytes.Buffer{}, false)
	s.logPath = "/state/last-sync.log"
	// Fed from the stream's own counters, the values emit passes at the real call sites.
	s.sent, s.errors = 396, 4
	renderings := []string{
		s.notice(),
		barFrame("claude-code-transcripts", 412, 1200, s.sent, s.errors),
		plainFrame("claude-code-transcripts", 412, 1200, s.sent, s.errors),
	}
	for _, r := range renderings {
		fields := strings.Fields(r)
		for i := 0; i+1 < len(fields); i++ {
			switch fields[i] {
			case "shipped", "unchanged", "skipped", "parked", "failed":
				var n int
				if _, err := fmt.Sscanf(fields[i+1], "%d", &n); err == nil {
					t.Errorf("%q reads as a run summary at %q %q", r, fields[i], fields[i+1])
				}
			}
		}
	}
}

// A long source id must not push the frame past the bound: \r cannot erase a wrapped line.
func TestALongSourceIDIsShortenedRatherThanWrapped(t *testing.T) {
	frame := barFrame(strings.Repeat("s", 200), 5, 10, 5, 0)
	if len(frame) > barCols {
		t.Errorf("frame is %d columns, over the %d bound: %q", len(frame), barCols, frame)
	}
	if !strings.Contains(frame, "5/10") {
		t.Errorf("clamping ate the counters: %q", frame)
	}
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}

	regular := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(regular)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file is not a terminal")
	}

	// /dev/null is the known imprecision of a character-device check, and it is accepted.
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s here: %v", os.DevNull, err)
	}
	defer null.Close()
	if !isTerminal(null) {
		t.Errorf("%s is a character device; the check is meant to say so", os.DevNull)
	}
}
