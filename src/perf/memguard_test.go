//go:build perf

// The memory guard: what a sync costs in resident bytes, and what stops it costing more. Two layers
// must hold: the harness fails a run that merely reached its budget, and a Linux cgroup underneath
// makes a runaway a clean SIGKILL. The fixtures are incompressible on purpose (see memguardRun).
package perf

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Spelled out rather than imported, because this module consumes the shipper as a binary;
// assertInFlightCapIsWired is what catches drift in the literal.
const envMaxInFlightBytes = "SHIPPER_MAX_IN_FLIGHT_BYTES"

// S3's acceptance gate: the requirement, not a measurement; see smokeBigFileAcceptance.
const (
	acceptanceFileBytes int64 = 500 << 20
	acceptanceBudget    int64 = 200 << 20
)

// S3's interim guard: 32 MiB rather than the design's 100 MB, because the tier's budget cannot afford
// a seventy second scenario. That costs resolution, so it catches a doubling rather than one extra
// copy. The budget is PROVISIONAL; tightening is the direction, loosening one to pass is not.
const (
	bigFileBytes  int64 = 32 << 20
	bigFileBudget int64 = 416 << 20
)

// S4: four files, no two of which fit under the cap together, so admission must serialise them or the
// peak gives it away. An override rather than the compiled default, which would mean staging gigabytes.
const (
	inFlightCap       int64 = 12 << 20
	inFlightFileBytes int64 = 8 << 20
	inFlightFiles           = 4

	// Sits between the serialised peak and the peak the same files reach when allowed to stack, so a
	// gate that stopped serialising fails here rather than merely running faster.
	inFlightBudget int64 = 208 << 20
)

// PROVISIONAL on the same terms as bigFileBudget, and gating for the first time where VmHWM exists.
const (
	singleLineFileBytes int64 = 50 << 20
	singleLineBudget    int64 = 250 << 20
	// A dirty string exists as source, decoded text, replacement and output at once; bounding that
	// corner is deliberate, rather than forcing a chunked matcher into the normal path.
	singleLineDirtyBudget int64 = 320 << 20
)

// GOMEMLIMIT is a soft target and can never fail a test. It sits under the cap so the runtime spends
// CPU before the kernel spends the process, and under the cap rather than under the budget, which
// would only buy collector time.
const memoryLimitNumerator, memoryLimitDenominator = 3, 4

// The kernel's ceiling sits above the gated budget so the high-water assertion speaks first, with a
// peak and a budget in the message; a cap at the budget turns every small breach into a bare SIGKILL.
const memoryCapHeadroom = 2

// --- S3: one big file --------------------------------------------------------

// The requirement, skipped because it is not met: the pipeline holds the payload about five times
// over, so this OOMs by construction. It is the acceptance test for the streaming change, and
// deleting the skip is what that change has to do; the body stays compiled so it cannot rot.
func smokeBigFileAcceptance(t *testing.T) {
	t.Run("S3-big-file-acceptance", func(t *testing.T) {
		t.Skipf("the acceptance gate for streaming: %d bytes through a %d byte ceiling needs a "+
			"pipeline that does not hold the payload five times over. Remove this skip with the "+
			"streaming change, not before", acceptanceFileBytes, acceptanceBudget)

		requireMemoryCap(t)
		w := stageCappedWorld(t, acceptanceBudget)
		// The compiled 256 MiB source ceiling would skip this fixture rather than ship it.
		raiseMaxFileBytes(t, w, acceptanceFileBytes*2)
		staged := stageIncompressibleFile(t, w, 0, acceptanceFileBytes)
		runUnderBudget(t, w, acceptanceScenario, resourceBudget{memory: acceptanceBudget}, 1, staged)
	})
}

// The interim guard, so the number lands in the results file every run. One file rather than a corpus:
// the per-file copies are what this scenario is about.
func smokeBigFile(t *testing.T) {
	t.Run("S3-big-file", func(t *testing.T) {
		requireMemoryCap(t)
		w := stageCappedWorld(t, bigFileBudget)
		staged := stageIncompressibleFile(t, w, 0, bigFileBytes)
		runUnderBudget(t, w, bigFileScenario, resourceBudget{memory: bigFileBudget}, 1, staged)
	})
}

// --- S4: the in-flight cap ---------------------------------------------------

// End-to-end proof that the admission gate serialises large files: the unit test says the arithmetic
// is right and nothing about whether a real sync obeys it. No cgroup and no GOMEMLIMIT here,
// deliberately: a hard cap would turn a broken gate into a kill and a soft limit would hide one.
func smokeInFlightCap(t *testing.T) {
	t.Run("S4-in-flight-cap", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS
		w.extraEnv = append(w.extraEnv,
			fmt.Sprintf("%s=%d", envMaxInFlightBytes, inFlightCap))

		assertInFlightCapIsWired(t, w)

		staged := 0
		for i := range inFlightFiles {
			staged += stageIncompressibleFile(t, w, i, inFlightFileBytes)
		}
		t.Logf("%s: %d files of %d bytes under a %d byte in-flight cap, so no two can be "+
			"admitted together", inFlightScenario, inFlightFiles, inFlightFileBytes, inFlightCap)

		runUnderBudget(t, w, inFlightScenario, resourceBudget{memory: inFlightBudget}, inFlightFiles, staged)
	})
}

// The deterministic half of the scenario's claim, since a peak is a number with a spread: a binary
// that refuses a non-numeric cap and names the variable is a binary that read it.
func assertInFlightCapIsWired(t *testing.T, w *world) {
	t.Helper()
	// A copy, so the probe's deliberately broken value never reaches the measured run.
	probe := *w
	probe.extraEnv = append(slices.Clone(w.extraEnv), envMaxInFlightBytes+"=not-a-byte-count")

	obs := probe.observedSync(t)
	if obs.Err == nil {
		t.Fatalf("a sync with %s set to a non-number ended %s: the cap this scenario sets is not "+
			"being read, so the peak below would describe the compiled default instead\n%s",
			envMaxInFlightBytes, obs.exitStatus(), obs.Output)
	}
	if !strings.Contains(obs.Output, envMaxInFlightBytes) {
		t.Fatalf("a sync with %s set to a non-number ended %s without naming it:\n%s",
			envMaxInFlightBytes, obs.exitStatus(), obs.Output)
	}
}

// --- S6 and S7: the single-line transcripts ----------------------------------

// One 50 MiB JSON string: the shape that forces the scrubber to hold a whole decoded value. No CPU
// budget on either single-line scenario, deliberately, because none has ever been calibrated.
func smokeSingleLine(t *testing.T) {
	t.Run("S6-single-line-transcript", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS
		staged := stageSingleLineFile(t, w, singleLineFileBytes)
		runUnderBudget(t, w, singleLineScenario, resourceBudget{memory: singleLineBudget}, 1, staged)
	})
}

// The same giant string carrying secrets, so decoded text, replacement and re-quoted output all exist
// at once: the corner singleLineDirtyBudget is deliberately higher for.
func smokeSingleLineSecrets(t *testing.T) {
	t.Run("S7-single-line-secret-transcript", func(t *testing.T) {
		w := stageWorld(t)
		w.gomaxprocs = smokeGOMAXPROCS
		staged, secrets := stageSingleLineSecretFile(t, w, singleLineFileBytes)
		runUnderBudget(t, w, singleLineSecretScenario,
			resourceBudget{memory: singleLineDirtyBudget}, 1, staged)

		assertRuleHits(t, w, corpusSessionID(0), map[string]int{"github-pat": secrets})
	})
}

// --- the shared run ----------------------------------------------------------

// What a scenario declares it must stay under; zero on a dimension means unmonitored, so a scenario
// gates on what it was written to measure and records the rest.
type resourceBudget struct {
	memory int64 // bytes of peak RSS, gated on every run

	// Child CPU seconds: recorded on every row but judged by the declaring scenario on the
	// repetition it chooses, since a per-run gate hands the verdict to the noisiest one.
	cpu float64
}

// The two memory layers fail differently: a SIGKILL is the cgroup's runaway verdict, and a survivor
// that reached the budget fails just as hard. The wire-byte floor is what makes the payload real: a
// fixture that compressed away would give a fast green run proving nothing about the size it claimed.
func runUnderBudget(t *testing.T, w *world, scenario string, budget resourceBudget, files, logical int) childObservation {
	t.Helper()

	var obs childObservation
	c := aroundStore(t, func() { obs = w.observedSync(t) })
	objects := len(w.currentKeys(t))

	t.Logf("%s: %d files, %d logical bytes, budget %d: %s in %v, peak %d bytes (%s), "+
		"%.2f cpu seconds, %d up and %d down (%.3f wire ratio), %d S3 requests, %d objects",
		scenario, files, logical, budget.memory, obs.exitStatus(), obs.Elapsed.Round(time.Millisecond),
		obs.PeakRSS, obs.PeakRSSSource, obs.CPUSeconds, c.up, c.down,
		float64(c.up+c.down)/float64(logical), c.requests, objects)

	// Recorded before anything is judged: a breach with no row is one nobody can calibrate against.
	recordResult(t, perfResult{
		Scenario:        scenario,
		CorpusFiles:     files,
		CorpusBytes:     logical,
		ChildGOMAXPROCS: w.gomaxprocs,
		// These are not timed against it, but a row that hid the shaping would be compared with
		// one measured on an unshaped wire.
		ShapedRTTMillis:   smokeRTT.Milliseconds(),
		RepSeconds:        []float64{obs.Elapsed.Seconds()},
		BestSeconds:       obs.Elapsed.Seconds(),
		ProxiedBytesUp:    c.up,
		ProxiedBytesDown:  c.down,
		S3Requests:        c.requests,
		Objects:           objects,
		PeakRSSBytes:      obs.PeakRSS,
		PeakRSSSource:     obs.PeakRSSSource,
		MemoryBudgetBytes: budget.memory,
		CPUSeconds:        obs.CPUSeconds,
		CPUBudgetSeconds:  budget.cpu,
		ExitStatus:        obs.exitStatus(),
	})

	// The timeout first: both arrive as a SIGKILL and only one of them is about memory.
	if obs.TimedOut {
		t.Fatalf("%s: the sync did not finish inside %v and was killed for running long, not by "+
			"its %d byte cap; peak read %d bytes (%s)",
			scenario, syncTimeout, budget.memory, obs.PeakRSS, obs.PeakRSSSource)
	}
	// Not necessarily this scenario's ceiling: S4 runs uncapped, so a kill there came from the machine.
	if obs.Killed() {
		t.Fatalf("%s: the child was killed carrying %d bytes of fixture in %d files under a %d "+
			"byte budget; peak read %d bytes (%s) before it went",
			scenario, logical, files, budget.memory, obs.PeakRSS, obs.PeakRSSSource)
	}
	if obs.Err != nil {
		t.Fatalf("%s: shipper run --once ended %s: %v\n%s", scenario, obs.exitStatus(), obs.Err, obs.Output)
	}

	counts := summary(t, obs.Output)
	if counts["shipped"] < files {
		t.Errorf("%s: the run shipped %d of %d files:\n%s", scenario, counts["shipped"], files, obs.Output)
	}
	if counts["failed"] != 0 {
		t.Errorf("%s: the run failed %d files:\n%s", scenario, counts["failed"], obs.Output)
	}
	if want := files + smokeSidecarObjects; objects != want {
		t.Errorf("%s: %d objects under %s, want exactly %d (%d files and %d sidecars)",
			scenario, objects, w.keyRoot, want, files, smokeSidecarObjects)
	}
	if floor := int64(logical) / 2; c.up < floor {
		t.Errorf("%s: %d bytes went up for %d logical bytes, under the %d floor: the fixture "+
			"compressed away and the run proves nothing about carrying %d bytes",
			scenario, c.up, logical, floor, logical)
	}

	assertPeakUnderBudget(t, scenario, obs, budget.memory)
	assertNoResidualScratch(t, w)
	return obs
}

// Three layers from one budget, in the order they should speak: the harness fails at the budget, the
// runtime targets three quarters of the kernel ceiling, and the kernel kills at memoryCapHeadroom
// times it. The launcher carries a snapshot of the world's variables, because sudo will not carry the
// environment across, so a scenario must not change the machine after this returns.
func stageCappedWorld(t *testing.T, budget int64) *world {
	t.Helper()
	w := stageWorld(t)
	w.gomaxprocs = smokeGOMAXPROCS
	hard := budget * memoryCapHeadroom
	w.extraEnv = append(w.extraEnv,
		fmt.Sprintf("GOMEMLIMIT=%d", hard*memoryLimitNumerator/memoryLimitDenominator))
	w.launcher = memoryCapArgs(hard, w.childVars())
	return w
}

// Appended to the config the world already wrote: one scenario needs it, and a source ceiling is a
// property of that fixture rather than of the machine.
func raiseMaxFileBytes(t *testing.T, w *world, limit int64) {
	t.Helper()
	path := filepath.Join(w.Config, "trajectory-shipper", "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the world's config: %v", err)
	}
	body = append(body, fmt.Sprintf(
		"sources:\n  - id: claude-code-transcripts\n    max_file_bytes: %d\n", limit)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("raise max_file_bytes to %d: %v", limit, err)
	}
}

// --- the hard cap ------------------------------------------------------------

// No memory controller here means no enforceable ceiling, whatever systemd-run reports.
const cgroupControllers = "/sys/fs/cgroup/cgroup.controllers"

// Small enough to make the hog cheap, large enough that a Go runtime starting up is nowhere near it.
const memoryCapProbeBytes int64 = 128 << 20

// Bounded on purpose: a hog that grew until something stopped it is the failure this suite prevents,
// so the probe asks only for what it can afford on a machine where the cap turns out to be decoration.
const memoryHogOvershoot = 4

// Touching 512 MiB takes well under a second, so a probe still going has found something to say.
const memoryCapProbeTimeout = 2 * time.Minute

// Separates "this developer's machine cannot cap memory" from "the machine this tier targets cannot".
const ciEnv = "GITHUB_ACTIONS"

var (
	capOnce sync.Once
	capErr  error
)

// Skipped loudly rather than run uncapped: a memory guard whose cap silently was not there passes for
// the wrong reason. On CI it is a failure and not a skip, because the runner is the machine these
// budgets are calibrated on, and a lost sudo would drop the whole scenario out of a green job.
func requireMemoryCap(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the memory cap is a cgroup and %s has none: this scenario needs Linux", runtime.GOOS)
	}
	capOnce.Do(func() { capErr = probeMemoryCap() })
	if capErr == nil {
		return
	}
	if os.Getenv(ciEnv) == "true" {
		t.Fatalf("no enforceable memory cap on this CI runner, where it is a precondition "+
			"rather than an option: %v", capErr)
	}
	t.Skipf("no enforceable memory cap on this machine: %v", capErr)
}

// Four things, each of which has failed somewhere: the memory controller must exist, systemd-run must
// accept the properties, the command must come back as the calling user (without --uid it runs as root
// and leaves a world the framework cannot delete), and the cap must actually kill something.
func probeMemoryCap() error {
	controllers, err := os.ReadFile(cgroupControllers)
	if err != nil {
		return fmt.Errorf("no cgroup v2 at %s: %v", cgroupControllers, err)
	}
	if !slices.Contains(strings.Fields(string(controllers)), "memory") {
		return fmt.Errorf("%s offers no memory controller, only %q",
			cgroupControllers, strings.TrimSpace(string(controllers)))
	}
	for _, tool := range []string{"sudo", "systemd-run"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("no %s on this machine: %v", tool, err)
		}
	}
	id, err := exec.LookPath("id")
	if err != nil {
		return fmt.Errorf("no id(1) to probe the wrapper with: %v", err)
	}

	argv := append(memoryCapArgs(memoryCapProbeBytes, nil), id, "-u")
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	// The last line: sudo and systemd-run write their own noise to stderr.
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return fmt.Errorf("%s printed nothing", strings.Join(argv, " "))
	}
	if got, want := fields[len(fields)-1], strconv.Itoa(os.Getuid()); got != want {
		return fmt.Errorf("a capped child ran as uid %s, not %s: it would leave a world this "+
			"test cannot clean up", got, want)
	}
	return probeMemoryCapKills()
}

// The half of the probe nothing else can stand in for: a ceiling the kernel accepts but does not
// enforce produces a scope that starts, runs and caps nothing, so something deliberately over-budget
// has to die inside one. The hog is this test binary, whose appetite the harness knows.
func probeMemoryCapKills() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("no path to this test binary to run a hog with: %v", err)
	}
	want := memoryCapProbeBytes * memoryHogOvershoot

	ctx, cancel := context.WithTimeout(context.Background(), memoryCapProbeTimeout)
	defer cancel()
	argv := append(memoryCapArgs(memoryCapProbeBytes, []string{
		fmt.Sprintf("%s=%d", memoryHogEnv, want),
	}), self)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = waitDelay
	out, err := cmd.CombinedOutput()

	if err == nil {
		return fmt.Errorf("a child that touched %d bytes under a %d byte cap exited cleanly: "+
			"the cap is accepted and not enforced, so nothing below it would be capped either",
			want, memoryCapProbeBytes)
	}
	st := cmd.ProcessState
	if st == nil {
		return fmt.Errorf("the hog left no process state behind: %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	ws, ok := st.Sys().(syscall.WaitStatus)
	killed := (ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL) ||
		st.ExitCode() == 128+int(syscall.SIGKILL)
	if !killed {
		return fmt.Errorf("a child that touched %d bytes under a %d byte cap ended %v rather "+
			"than being killed: %s", want, memoryCapProbeBytes, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- the hog ------------------------------------------------------------------

// Puts this test binary into hog mode, as a whole number of bytes to touch.
const memoryHogEnv = "SHIPPER_PERF_MEMORY_HOG_BYTES"

// The thing the memory cap is supposed to kill. Touched a page at a time rather than merely
// allocated: an untouched allocation is address space, and the cgroup accounts pages.
func runMemoryHog(spec string) int {
	want, err := strconv.ParseInt(strings.TrimSpace(spec), 10, 64)
	if err != nil || want <= 0 {
		fmt.Fprintf(os.Stderr, "perf: %s=%q is not a positive whole number of bytes\n",
			memoryHogEnv, spec)
		return 2
	}
	const (
		chunk = 1 << 20
		page  = 4 << 10
	)
	held := make([][]byte, 0, want/chunk+1)
	for total := int64(0); total < want; total += chunk {
		buf := make([]byte, chunk)
		for i := 0; i < len(buf); i += page {
			buf[i] = 1
		}
		held = append(held, buf)
	}
	// Held to the end: memory the collector could take back is not something a cap has to kill.
	runtime.KeepAlive(held)
	return 0
}

// A transient scope rather than a container, so the child stays a direct descendant of the test
// process. MemorySwapMax=0 is not optional: with a runner's swapfile unbounded, a cap becomes a
// thrashing machine instead of a clean kill, and OOMPolicy=kill takes the whole scope at once. The
// environment travels as --setenv because sudo resets what it was called with.
func memoryCapArgs(limit int64, vars []string) []string {
	argv := []string{
		"sudo", "-n",
		"systemd-run", "--scope", "--quiet",
		fmt.Sprintf("--uid=%d", os.Getuid()),
		fmt.Sprintf("--gid=%d", os.Getgid()),
		fmt.Sprintf("--property=MemoryMax=%d", limit),
		"--property=MemorySwapMax=0",
		"--property=OOMPolicy=kill",
	}
	for _, v := range vars {
		argv = append(argv, "--setenv="+v)
	}
	return append(argv, "--")
}

// --- the incompressible fixture ----------------------------------------------

// A literal, never a clock: two runs writing different bytes would report different memory.
const memguardSeedText = "trajectory-shipper perf memguard"

// Exactly the 32 bytes ChaCha8 takes, checked while compiling: a longer seed truncates silently.
const (
	_ = uint(len(memguardSeedText) - 32)
	_ = uint(32 - len(memguardSeedText))
)

var memguardSeed = [32]byte([]byte(memguardSeedText))

const (
	// 64 symbols, so one random byte masked to six bits picks one.
	memguardAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	// A redaction constraint, not a cosmetic one: the entropy backstop replaces any run of 24 or
	// more base64-alphabet characters, so a space every twenty keeps the staged bytes the shipped
	// bytes and this file out of the redaction path.
	memguardRun = 20

	// Large enough that the JSON frame is noise, small enough that generating a file stays a stream.
	memguardLineFill = 8 << 10
)

// Streamed through a buffered writer, never assembled in memory: this measures what the child holds,
// and a harness building a 500 MB string first would be the biggest process in the measurement.
func stageIncompressibleFile(t *testing.T, w *world, index int, target int64) int {
	t.Helper()
	written, _ := stageIncompressibleValue(t, w, index, target, 0)
	return written
}

// The writer behind it; with secretEvery above zero it plants a secret pair every secretEvery-th line
// and reports how many carry one, so a run's redaction count can be checked exactly.
func stageIncompressibleValue(t *testing.T, w *world, index int, target int64, secretEvery int) (int, int) {
	t.Helper()
	slug := fmt.Sprintf("-Users-perf-work-bulk-%d", index)
	session := corpusSessionID(index)
	cwd := "/Users/perf/work/" + strings.TrimPrefix(slug, "-Users-perf-work-")
	path := filepath.Join(w.Home, ".claude", "projects", slug, session+".jsonl")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)

	written := 0
	n, err := fmt.Fprintf(bw, corpusFirstLine, session, cwd)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	written += n

	src := rand.NewChaCha8(memguardStreamSeed(index))
	fill := make([]byte, memguardLineFill)
	// A tail after a space rather than a splice: an insertion would break the filler alignment that
	// keeps the entropy backstop off it (see memguardRun). Bare tokens, because a KEY=VALUE frame can
	// route a hit to a key-name rule, and the trailing words keep either token off the string boundary.
	secretTail := []byte(" " + corpusGitHubToken + " " + corpusAWSKey + " end of line")
	text := make([]byte, 0, memguardLineFill+len(secretTail))
	secretLines := 0
	for line := 1; int64(written) < target; line++ {
		memguardFill(t, src, fill)
		text = append(text[:0], fill...)
		if secretEvery > 0 && line%secretEvery == 0 {
			text = append(text, secretTail...)
			secretLines++
		}
		n, err := fmt.Fprintf(bw, `{"type":"assistant","uuid":"a%d","sessionId":%q,"cwd":%q,"message":{"id":"m%d","model":"claude-opus-5","content":[{"type":"text","text":%q}],"usage":{"input_tokens":120,"output_tokens":340}}}`+"\n",
			line, session, cwd, line, text)
		if err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		written += n
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	// The pre-filter compares size and mtime, so the stamp cannot be the wall clock.
	if err := os.Chtimes(path, fixtureMTime, fixtureMTime); err != nil {
		t.Fatal(err)
	}
	return written, secretLines
}

const singleLineChunk = 390 * (memguardRun + 1)

func stageSingleLineFile(t *testing.T, w *world, target int64) int {
	t.Helper()
	written, _ := stageSingleLineValue(t, w, "-Users-perf-work-oneline", target, "")
	return written
}

func stageSingleLineSecretFile(t *testing.T, w *world, target int64) (int, int) {
	t.Helper()
	return stageSingleLineValue(t, w, "-Users-perf-work-oneline-secrets", target, corpusGitHubToken)
}

func stageSingleLineValue(t *testing.T, w *world, slug string, target int64, secret string) (int, int) {
	t.Helper()
	session := corpusSessionID(0)
	cwd := "/Users/perf/work/" + strings.TrimPrefix(slug, "-Users-perf-work-")
	path := filepath.Join(w.Home, ".claude", "projects", slug, session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)

	tail := `"}],"usage":{"input_tokens":120,"output_tokens":340}}}` + "\n"
	written, err := fmt.Fprintf(bw, `{"type":"assistant","uuid":"a1","sessionId":%q,"cwd":%q,"message":{"id":"m1","model":"claude-opus-5","content":[{"type":"text","text":"`, session, cwd)
	if err != nil {
		t.Fatal(err)
	}
	src := rand.NewChaCha8(memguardStreamSeed(0))
	fill := make([]byte, singleLineChunk)
	secrets := 0
	for int64(written+len(tail)) < target {
		if secret != "" {
			n, err := bw.WriteString(secret + " ")
			if err != nil {
				t.Fatal(err)
			}
			written += n
			secrets++
		}
		memguardFill(t, src, fill)
		n, err := bw.Write(fill)
		if err != nil {
			t.Fatal(err)
		}
		written += n
	}
	n, err := bw.WriteString(tail)
	if err != nil {
		t.Fatal(err)
	}
	written += n
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixtureMTime, fixtureMTime); err != nil {
		t.Fatal(err)
	}
	return written, secrets
}

// A stream per file, so a store that deduplicated identical payloads could not make this look cheap.
func memguardStreamSeed(index int) [32]byte {
	seed := memguardSeed
	seed[len(seed)-1] ^= byte(index)
	seed[len(seed)-2] ^= byte(index >> 8)
	return seed
}

// High-entropy text with a space every memguardRun characters; see memguardRun for why.
func memguardFill(t *testing.T, src *rand.ChaCha8, buf []byte) {
	t.Helper()
	if _, err := src.Read(buf); err != nil {
		t.Fatalf("read the fixture stream: %v", err)
	}
	for i := range buf {
		if i%(memguardRun+1) == memguardRun {
			buf[i] = ' '
			continue
		}
		buf[i] = memguardAlphabet[buf[i]&63]
	}
}
