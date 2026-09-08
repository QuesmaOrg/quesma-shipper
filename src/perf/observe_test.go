//go:build perf

// What a child run cost, and what it left behind. The shipper runs as an ordinary child, so peak
// memory, CPU and exit status are three syscalls away. Peak memory is read two ways (VmHWM cannot
// undercount but misses the last moments; Maxrss arrives only after exit) and the larger is gated.
package perf

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Not a sampling rate: VmHWM only ever grows, so this decides how likely a read lands before exit.
const vmHWMInterval = 20 * time.Millisecond

// Without it the deadline is not one: Wait blocks on the output pipe copy, and cancelling the
// context kills only the direct child, which for a capped run is sudo rather than the shipper.
const waitDelay = 30 * time.Second

// One run of the shipper, measured rather than judged; nothing here fails a test.
type childObservation struct {
	Output  string
	Elapsed time.Duration

	// The larger of the two readings and which one it was; VmHWM is zero where /proc is not.
	PeakRSS       int64
	PeakRSSSource string

	// User plus system time: the "did it hang or did it work" figure.
	CPUSeconds float64

	ExitCode int
	Signal   syscall.Signal

	// Separates a syncTimeout kill from the SIGKILL a scenario was expecting.
	TimedOut bool

	// What Wait returned: nil for a clean exit, non-nil for anything else.
	Err error
}

// The one-word form that goes into the results file.
func (o childObservation) exitStatus() string {
	switch {
	case o.Signal != 0:
		return "signal " + o.Signal.String()
	case o.ExitCode == 0:
		return "ok"
	default:
		return fmt.Sprintf("exit %d", o.ExitCode)
	}
}

// Two shapes, because a wrapper reports its child's death the way a shell does, as 128 plus the
// signal. A syncTimeout kill looks identical from here, so a scenario reads TimedOut first.
func (o childObservation) Killed() bool {
	return o.Signal == syscall.SIGKILL || o.ExitCode == 128+int(syscall.SIGKILL)
}

// The wall clock covers the whole process, startup included: skipping it would flatter every
// steady-state result.
func (w *world) observedSync(t *testing.T) childObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()

	argv := append(slices.Clone(w.launcher), w.binary, "run", "--once")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = w.childEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// See waitDelay: the deadline above only ends the run because of this line.
	cmd.WaitDelay = waitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the shipper: %v", err)
	}
	peak := watchPeakRSS(cmd.Process.Pid)
	err := cmd.Wait()
	vmHWM := peak()
	obs := childObservation{
		Output:   out.String(),
		Elapsed:  time.Since(start),
		TimedOut: ctx.Err() != nil,
		Err:      err,
	}

	st := cmd.ProcessState
	if st == nil {
		t.Fatalf("the shipper left no process state behind: %v", err)
	}
	obs.ExitCode = st.ExitCode()
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		obs.Signal = ws.Signal()
	}
	ru, ok := st.SysUsage().(*syscall.Rusage)
	if !ok {
		t.Fatalf("no rusage for the child on %s: peak memory cannot be observed", runtime.GOOS)
	}
	obs.CPUSeconds = seconds(ru.Utime) + seconds(ru.Stime)

	obs.PeakRSS, obs.PeakRSSSource = ru.Maxrss*maxrssUnit(t), "Maxrss"
	if vmHWM > obs.PeakRSS {
		obs.PeakRSS, obs.PeakRSSSource = vmHWM, "VmHWM"
	}
	return obs
}

func seconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}

// Kilobytes on Linux, bytes on darwin, a loud stop elsewhere rather than a peak off by 1000x.
func maxrssUnit(t *testing.T) int64 {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return 1 << 10
	case "darwin":
		return 1
	default:
		t.Fatalf("the unit of Rusage.Maxrss on %s is not known here", runtime.GOOS)
		return 0
	}
}

// Returns a function that stops the watch and reports it; zero where /proc is not. Nothing may be
// read after the stop: a reaped pid's numbers are gone or belong to someone else.
func watchPeakRSS(pid int) func() int64 {
	if runtime.GOOS != "linux" {
		return func() int64 { return 0 }
	}
	done := make(chan struct{})
	result := make(chan int64, 1)
	go func() {
		var peak int64
		read := func() {
			if v := peakVmHWM(pid); v > peak {
				peak = v
			}
		}
		tick := time.NewTicker(vmHWMInterval)
		defer tick.Stop()
		for {
			read()
			select {
			case <-done:
				result <- peak
				return
			case <-tick.C:
			}
		}
	}()

	var (
		once sync.Once
		peak int64
	)
	return func() int64 {
		once.Do(func() {
			close(done)
			peak = <-result
		})
		return peak
	}
}

// The subtree and not the process: a capped run has wrappers in between, and the reading worth
// having is the deepest one, which is also the largest by orders of magnitude.
func peakVmHWM(pid int) int64 {
	peak, _ := readVmHWM(pid)
	for _, child := range procChildren(pid) {
		if v := peakVmHWM(child); v > peak {
			peak = v
		}
	}
	return peak
}

// A process's direct children; empty where there are none and where /proc is not.
func procChildren(pid int) []int {
	tasks := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(tasks, e.Name(), "children"))
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(raw)) {
			if child, err := strconv.Atoi(field); err == nil {
				out = append(out, child)
			}
		}
	}
	return out
}

// Not ok once the process is gone, which is expected rather than an error.
func readVmHWM(pid int) (int64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "VmHWM:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}

// --- the memory budget -------------------------------------------------------

// Touching the budget is a failure, killed or not. A hard gate only on Linux, where VmHWM does not
// depend on when the harness looked; elsewhere the log says the gate did not run.
func assertPeakUnderBudget(t *testing.T, scenario string, obs childObservation, budget int64) {
	t.Helper()
	if budget <= 0 {
		t.Fatalf("%s declared a memory budget of %d bytes; a budget is a positive number of bytes",
			scenario, budget)
	}
	// A reading of zero is under every budget there is: a broken instrument must not pass a gate.
	if obs.PeakRSS <= 0 {
		t.Errorf("%s: peak RSS read as %d bytes from %s: the instrument is what this gate would "+
			"be passing, not the run", scenario, obs.PeakRSS, obs.PeakRSSSource)
		return
	}
	if runtime.GOOS != "linux" {
		t.Logf("%s: peak %d bytes (%s) against a %d byte budget, recorded only: "+
			"the memory gate needs VmHWM and %s has no /proc",
			scenario, obs.PeakRSS, obs.PeakRSSSource, budget, runtime.GOOS)
		return
	}
	if obs.PeakRSS >= budget {
		t.Errorf("%s: peak RSS %d bytes (%s) reached its %d byte budget",
			scenario, obs.PeakRSS, obs.PeakRSSSource, budget)
		return
	}
	t.Logf("%s: peak RSS %d bytes (%s), %d bytes under the %d byte budget",
		scenario, obs.PeakRSS, obs.PeakRSSSource, budget-obs.PeakRSS, budget)
}

// --- the CPU budget ----------------------------------------------------------

// A zero budget is a scenario that made no CPU claim, not a broken one; where the memory gate treats
// zero as a mistake, this one returns. Unlike that gate it runs on every platform, so the budget has
// to hold on the slowest machine that runs it. Reaching the budget is a failure.
func assertCPUUnderBudget(t *testing.T, scenario string, obs childObservation, budget float64) {
	t.Helper()
	if budget <= 0 {
		return
	}
	// A reading of zero is under every budget there is; a child that ran burned CPU.
	if obs.CPUSeconds <= 0 {
		t.Errorf("%s: child CPU read as %.3f seconds: the instrument is what this gate would "+
			"be passing, not the run", scenario, obs.CPUSeconds)
		return
	}
	if obs.CPUSeconds >= budget {
		t.Errorf("%s: the child burned %.2f CPU seconds, reaching its %.2f second budget",
			scenario, obs.CPUSeconds, budget)
		return
	}
	t.Logf("%s: %.2f CPU seconds, %.2f under the %.2f second budget",
		scenario, obs.CPUSeconds, budget-obs.CPUSeconds, budget)
}

// --- what the run left behind ------------------------------------------------

// Every durable local write is temp-then-rename, so a temp file that outlived the process is a
// write that never committed.
func assertNoResidualScratch(t *testing.T, w *world) {
	t.Helper()
	var leftover []string
	for _, root := range []string{w.State, w.Config, w.Home} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && isScratchName(d.Name()) {
				leftover = append(leftover, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s for scratch files: %v", root, err)
		}
	}
	if len(leftover) == 0 {
		return
	}
	sort.Strings(leftover)
	shown := leftover
	if len(shown) > 10 {
		shown = shown[:10]
	}
	t.Errorf("a drained sync left %d scratch files behind, first %d: %s",
		len(leftover), len(shown), strings.Join(shown, " "))
}

// The shapes the shipper writes while a write is still in flight.
func isScratchName(name string) bool {
	return strings.Contains(name, ".tmp-") ||
		strings.HasSuffix(name, ".tmp") ||
		strings.HasSuffix(name, ".part") ||
		strings.HasSuffix(name, ".partial")
}

// --- the instruments, checked against a run that is not being measured ------

// Deliberately loose: this proves the gate reads a real number, so a tight bound here would only
// make the harness's own smoke check the flakiest thing in the tier.
const selfTestMemoryBudget = 512 << 20

// The tier's own smoke check: a helper nothing calls stops working quietly, and the scenarios that
// call these cost minutes each. One cheap sync exercises the whole chain.
func TestTheHarnessObservesAChildRun(t *testing.T) {
	w := stageWorld(t)
	files := smallCorpusFiles
	stageCorpusFiles(t, w, files)

	var obs childObservation
	c := aroundStore(t, func() { obs = w.mustSync(t) })

	if got := summary(t, obs.Output)["shipped"]; got < files {
		t.Fatalf("the self-test run shipped %d of %d files", got, files)
	}
	objects := len(w.currentKeys(t))

	if c.up <= 0 || c.down <= 0 {
		t.Errorf("the run shipped %d files and the proxy counted %d bytes up, %d down",
			files, c.up, c.down)
	}
	if c.requests <= 0 {
		t.Errorf("the run shipped %d files and MinIO counted %d new S3 requests", files, c.requests)
	}
	if objects < files {
		t.Errorf("the run shipped %d files and left %d objects under %s", files, objects, w.keyRoot)
	}
	if obs.PeakRSS <= 0 {
		t.Errorf("the child's peak RSS read as %d bytes from %s", obs.PeakRSS, obs.PeakRSSSource)
	}
	if obs.CPUSeconds <= 0 {
		t.Errorf("the child's CPU time read as %v seconds", obs.CPUSeconds)
	}

	assertPeakUnderBudget(t, selfTestScenario, obs, selfTestMemoryBudget)
	assertNoResidualScratch(t, w)

	recordResult(t, perfResult{
		Scenario:          selfTestScenario,
		CorpusFiles:       files,
		ChildGOMAXPROCS:   w.gomaxprocs,
		RepSeconds:        []float64{obs.Elapsed.Seconds()},
		BestSeconds:       obs.Elapsed.Seconds(),
		ProxiedBytesUp:    c.up,
		ProxiedBytesDown:  c.down,
		S3Requests:        c.requests,
		Objects:           objects,
		PeakRSSBytes:      obs.PeakRSS,
		PeakRSSSource:     obs.PeakRSSSource,
		MemoryBudgetBytes: selfTestMemoryBudget,
		CPUSeconds:        obs.CPUSeconds,
		ExitStatus:        obs.exitStatus(),
	})
}
