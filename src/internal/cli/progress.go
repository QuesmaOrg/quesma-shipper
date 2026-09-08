package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const (
	consoleLineBudget = 32

	plainEvery = 100

	runLogName = "last-sync.log"

	barCols = 75

	barWidth = 16
)

type progressStream struct {
	out     io.Writer // the console, always present: warnings go here even under --quiet
	log     io.Writer // nil until the log opens, and again if writing to it fails
	logFile *os.File  // the same file, kept so the stream can close what it opened
	logPath string
	quiet   bool
	tty     bool

	lines   int  // per-file lines already shown on the console
	noticed bool // whether the switch-over notice has been printed
	decided int
	sent    int
	errors  int

	frame string
}

func newProgressStream(out io.Writer, quiet bool) *progressStream {
	return &progressStream{
		out:   out,
		quiet: quiet,
		tty:   isTerminal(out),
	}
}

func (s *progressStream) openLog(stateDir string) {
	f, path, err := openRunLog(stateDir)
	if err != nil {
		printWarning(s.Stderr(), "no run log this time: "+err.Error())
		return
	}
	s.logFile, s.log, s.logPath = f, f, path
}

func (s *progressStream) closeLog() {
	if s.logFile == nil {
		return
	}
	s.logFile.Close()
	s.logFile, s.log = nil, nil
}

func openRunLog(stateDir string) (*os.File, string, error) {
	if err := platform.EnsureDir(stateDir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(stateDir, runLogName)
	f, err := platform.OpenTruncating(path, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

func (s *progressStream) emit(sourceID string, done, total int, f formats.FileOutcome) {
	line := progressLine(sourceID, done, total, f)
	if line != "" {
		s.tee(line)
	}
	s.decided++
	switch f.Decision {
	case formats.DecisionShipped:
		s.sent++
	case formats.DecisionParked, formats.DecisionFailed:
		s.errors++
	}
	if s.quiet {
		return
	}
	if s.lines < consoleLineBudget {
		if line == "" {
			return
		}
		fmt.Fprintln(s.out, line)
		s.lines++
		return
	}
	if line != "" && !s.noticed {
		s.noticed = true
		fmt.Fprintln(s.Stderr(), s.notice())
	}
	if s.tty {
		s.draw(barFrame(sourceID, done, total, s.sent, s.errors))
		return
	}
	if s.decided%plainEvery == 0 {
		fmt.Fprintln(s.out, plainFrame(sourceID, done, total, s.sent, s.errors))
	}
}

func (s *progressStream) Stderr() io.Writer { return barWriter{s} }

func (s *progressStream) Finish() { s.erase() }

func (s *progressStream) tee(line string) {
	if s.log == nil {
		return
	}
	if _, err := fmt.Fprintln(s.log, line); err != nil {
		s.log = nil
		fmt.Fprintf(s.Stderr(), "warning: the run log stopped at %s: %v\n", s.logPath, err)
	}
}

func (s *progressStream) notice() string {
	if s.logPath == "" {
		return fmt.Sprintf("  … %d lines shown; the rest of this run is in the summary below",
			consoleLineBudget)
	}
	return fmt.Sprintf("  … %d lines shown; the rest of this run is in %s",
		consoleLineBudget, s.logPath)
}

func (s *progressStream) draw(frame string) {
	pad := max(0, len(s.frame)-len(frame))
	fmt.Fprintf(s.out, "\r%s%s", frame, strings.Repeat(" ", pad))
	s.frame = frame
}

func (s *progressStream) erase() {
	if s.frame == "" {
		return
	}
	fmt.Fprintf(s.out, "\r%s\r", strings.Repeat(" ", len(s.frame)))
	s.frame = ""
}

type barWriter struct{ s *progressStream }

func (b barWriter) Write(p []byte) (int, error) {
	frame := b.s.frame
	b.s.erase()
	n, err := b.s.out.Write(p)
	if frame != "" {
		b.s.draw(frame)
	}
	return n, err
}

func barFrame(sourceID string, done, total, sent, errs int) string {
	filled := 0
	if total > 0 {
		filled = done * barWidth / total
	}
	filled = min(filled, barWidth)
	bar := "[" + strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled) + "]"
	return clampFrame(sourceID, fmt.Sprintf("  %s  %s", bar, counters(done, total, sent, errs)))
}

func plainFrame(sourceID string, done, total, sent, errs int) string {
	return clampFrame(sourceID, "  "+counters(done, total, sent, errs))
}

func counters(done, total, sent, errs int) string {
	return fmt.Sprintf("%d/%d  sent %d  errors %d", done, total, sent, errs)
}

func clampFrame(sourceID, tail string) string {
	room := max(0, barCols-len(tail))
	if len(sourceID) > room {
		sourceID = sourceID[:room]
	}
	return sourceID + tail
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
