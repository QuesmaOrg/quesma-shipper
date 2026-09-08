// Package crashjournal is the crash-safe run journal, and only that: start, the phase reached, exit, and
// the acknowledgement that a crash report was delivered. Death is detected by absence — a run whose
// entries never reach "exit" did not survive — which is the one thing no code running inside that
// run can report about itself.
//
// It deliberately does NOT journal failures the run survived, nor each file before reading it. The
// first is the failure record's job, which reaches a heartbeat where a journal entry never leaves
// the machine; the second bought the name of an OOM's suspect file at the price of a line per file,
// 92% of everything this wrote.
package crashjournal

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const fileName = "crash-journal.log"

const (
	maxLogBytes  = 1 << 20
	maxReadBytes = 4 << 20
)

// Every line carries its run id, so a daemon tick and a manual sync appending at once stay
// attributable.
type entry struct {
	At    time.Time `json:"at"`
	RunID string    `json:"run_id"`
	Ev    string    `json:"ev"`
	Phase string    `json:"phase,omitempty"`
	PID   int       `json:"pid,omitempty"`
}

// Log appends one run's entries. All methods are best-effort and nil-safe: the journal observes
// the run and must never stop it.
type Log struct {
	mu    sync.Mutex
	path  string
	runID string
}

// Open rotates a large journal aside and returns the appender. Rotation happens only here,
// between runs, so one run's entries never straddle generations.
func Open(stateDir, runID string) (*Log, error) {
	if err := platform.EnsureDir(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, fileName)
	if info, err := os.Stat(path); err == nil && info.Size() >= maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	return &Log{path: path, runID: runID}, nil
}

func (l *Log) Start() {
	l.append(entry{Ev: "start", PID: os.Getpid()}, true)
}

func (l *Log) Phase(name string) {
	l.append(entry{Ev: "phase", Phase: name}, false)
}

// Reported marks the pending crash report as DELIVERED: a heartbeat carrying it reached the sink.
// Exit alone cannot mean that — the heartbeat fails open, so a clean exit may still have shipped nothing.
func (l *Log) Reported() {
	l.append(entry{Ev: "reported"}, true)
}

func (l *Log) Exit() {
	l.append(entry{Ev: "exit"}, true)
}

// append fsyncs only the run markers: completed writes survive process death in the page cache,
// so the per-file path stays cheap and only start/exit pay for power-loss durability.
func (l *Log) append(e entry, syncNow bool) {
	if l == nil {
		return
	}
	e.At = time.Now().UTC()
	e.RunID = l.runID
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	body = append(body, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return
	}
	if syncNow {
		_ = f.Sync()
	}
}

// Summary reconstructs one past run from its entries.
type Summary struct {
	RunID    string
	PID      int
	Clean    bool // the run wrote an exit entry
	Reported bool // the run delivered the pending crash report in a heartbeat
	Phase    string

	// Crashes counts the runs that never reached exit since the last delivered report, this included.
	Crashes int
}

// LastRun reports the run before this process, or nil when the journal is absent or empty.
// Call it before Open: Open may rotate the very file this reads.
func LastRun(stateDir string) *Summary {
	raw, err := readCapped(filepath.Join(stateDir, fileName))
	if err != nil || len(raw) == 0 {
		return nil
	}

	byRun := map[string]*Summary{}
	var order []*Summary
	for len(raw) > 0 {
		line, rest, _ := bytes.Cut(raw, []byte{'\n'})
		raw = rest
		var e entry
		if json.Unmarshal(line, &e) != nil || e.RunID == "" {
			continue
		}
		s := byRun[e.RunID]
		if s == nil {
			s = &Summary{RunID: e.RunID}
			byRun[e.RunID] = s
			order = append(order, s)
		}
		switch e.Ev {
		case "start":
			s.Phase, s.PID = "start", e.PID
		case "phase":
			s.Phase = e.Phase
		case "exit":
			s.Clean = true
		case "reported":
			s.Reported = true
		}
	}
	if len(order) == 0 {
		return nil
	}

	// A crash must survive restarts that could not deliver the report: only a run that wrote
	// "reported" proved a heartbeat carrying it reached the sink, so the walk stops there and
	// nowhere else. A run with no exit whose process is still alive is concurrent, not dead.
	last := order[len(order)-1]
	var crash *Summary
	for i := len(order) - 1; i >= 0; i-- {
		s := order[i]
		if s.Reported {
			break
		}
		if s.Clean || alive(s.PID) {
			continue
		}
		if crash == nil {
			crash = s
		}
		crash.Crashes++
	}
	if crash != nil {
		return crash
	}
	if !last.Clean {
		return nil
	}
	return last
}

// readCapped reads at most the trailing maxReadBytes, dropping a leading partial line.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// A leading partial line needs no trimming: LastRun already skips anything that does not
	// unmarshal, which is the same tolerance that lets a torn last line through.
	if info.Size() > maxReadBytes {
		if _, err := f.Seek(-maxReadBytes, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
