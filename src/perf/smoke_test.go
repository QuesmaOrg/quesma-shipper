//go:build perf

// The tier CI runs: a three-minute budget on a shared runner makes it a different design, not a
// smaller copy. One machine, a tenth of the backlog, the child pinned to two cores, and the minimum
// of repetitions as the answer. Counters gate tightly, the clock second; the two tiers never compare.
package perf

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	// A tenth of the full tier's backlog; lower and the fixed costs of process start and staging
	// become a visible share of what is measured.
	smokeCorpusFiles = 1024

	// smokeGOMAXPROCS is the child's machine here. See the package comment.
	smokeGOMAXPROCS = 2

	// Matches the full tier, so both sets of logs describe the same link.
	smokeRTT = 50 * time.Millisecond

	// Fewer on the baseline on purpose: the bound is shaped minus unshaped, so an over-measured
	// baseline only makes the test easier to pass, and the shaped side is the one noise can fail.
	smokeBaselineReps = 2
	smokeReps         = 3

	// The engine's shape, and what S1's bound is derived from: a backlog runs in rounds of
	// smokeAdmissionFactor*GOMAXPROCS files, each PUT smokePutFanoutFactor*GOMAXPROCS wide with no
	// file admitted while a group is out. A pipeline that changes shape must move these constants.
	smokeAdmissionFactor = 5
	smokePutFanoutFactor = 4
)

// One admission round's files and the waves of PUTs it costs; their ratio is the overlap ceiling at
// this pin. It assumes a round fitting inside one authorization group, so it understates a larger pin.
const (
	smokeRoundFiles = smokeAdmissionFactor * smokeGOMAXPROCS
	smokePutWidth   = smokePutFanoutFactor * smokeGOMAXPROCS
	smokeRoundTrips = (smokeRoundFiles + smokePutWidth - 1) / smokePutWidth
)

// A heartbeat and a project map. Exact rather than a floor: a third per-sync object is the kind of
// change this tier exists to notice.
const smokeSidecarObjects = 2

// Measured, and narrow: two runs of unchanged code over this corpus agree to within an HTTP header.
const (
	// One S3 call per file plus the sidecars; the band catches both an extra call per file and a
	// run that quietly stopped making them, with room for the odd retry.
	smokeRequestsPerFileMin = 0.95
	smokeRequestsPerFileMax = 1.30

	// Wire bytes over logical: gates a run that started putting the corpus on the wire twice.
	smokeWireRatioMax = 0.85
)

// PROVISIONAL: roughly twice a laptop observation, which is headroom for a reading darwin cannot
// take rather than slack for the code, since the gate only runs where VmHWM exists. They say
// "nothing has grown much" and no more; loosening one to make a run pass is what they exist to stop.
const (
	smokeBacklogBudget int64 = 128 << 20
	smokeSteadyBudget  int64 = 96 << 20
)

// The one synthetic machine the whole tier runs on.
type smokeMachine struct {
	w     *world
	files int

	// The corpus on disk: the denominator of the wire ratio.
	bytes int
}

// The whole CI tier: one machine, one corpus, two scenarios. One test rather than two because S2
// measures the sync after S1's backlog, and its own world would mean shipping the backlog twice.
func TestSmokeTier(t *testing.T) {
	m := stageSmokeMachine(t)

	// The unshaped reference and the tier's warm-up, absorbing a fresh runner's cold caches once.
	baseline := smokeBaseline(t, m)

	withLatency(t, smokeRTT, 0)

	backlog := smokeBacklog(t, m, baseline)
	smokeSteady(t, m, backlog)

	// The resource guards last, on their own machines: they stage far bigger files and gate on
	// resident bytes and CPU rather than the clock, so they must not disturb the timed halves.
	smokeBigFileAcceptance(t)
	smokeBigFile(t)
	smokeInFlightCap(t)
	smokeScrubGuard(t)
	smokeSingleLine(t)
	smokeSingleLineSecrets(t)
}

// --- the unshaped reference --------------------------------------------------

// What the same backlog costs with nothing on the wire, so S1's bound can be about round trips:
// subtracting a baseline measured on the same core cancels the compute.
func smokeBaseline(t *testing.T, m *smokeMachine) time.Duration {
	t.Helper()
	m.w.reset(t)
	m.w.mustSync(t)

	reps := make([]time.Duration, 0, smokeBaselineReps)
	for range smokeBaselineReps {
		m.w.reset(t)
		obs := m.w.mustSync(t)
		if got := summary(t, obs.Output)["shipped"]; got < m.files {
			t.Fatalf("the baseline run shipped %d of %d files", got, m.files)
		}
		reps = append(reps, obs.Elapsed)
	}
	best := slices.Min(reps)
	t.Logf("unshaped baseline on %d files: reps %s, best %v",
		m.files, durationList(reps), best.Round(time.Millisecond))
	return best
}

// --- S1: the first sync ------------------------------------------------------

// What S2 needs from S1: the run it compares itself against.
type smokeBacklogResult struct {
	best     time.Duration
	bytes    int64
	versions map[string]int
}

// A cold first sync over the shaped link. Every repetition is cold: one against a warm state would
// be measuring S2 instead.
func smokeBacklog(t *testing.T, m *smokeMachine, baseline time.Duration) smokeBacklogResult {
	var result smokeBacklogResult
	t.Run("S1-backlog-first-sync", func(t *testing.T) {
		meas := smokeRepeat(t, m, coldRuns)
		counts := summary(t, meas.last.Output)
		moved := meas.counters.up + meas.counters.down
		result = smokeBacklogResult{
			best:     meas.best,
			bytes:    moved,
			versions: m.w.versionCounts(t),
		}

		// Half the overlap an admission round can reach, so a loaded runner has somewhere to go.
		// Derived from the round and not from the upload pool: on the vend path the pool is spent
		// per authorization group, so a bound written for a per-file pool describes a dead pipeline.
		added := meas.best - baseline
		serial := time.Duration(m.files) * smokeRTT
		bound := serial * 2 * smokeRoundTrips / smokeRoundFiles

		perFile := float64(meas.counters.requests) / float64(m.files)
		ratio := float64(moved) / float64(m.bytes)
		t.Logf("S1 backlog %d files at %v rtt: reps %s, best %v against a %v baseline, added %v "+
			"(bound %v on a serial %v, %.1fx overlap)",
			m.files, smokeRTT, durationList(meas.reps), meas.best.Round(time.Millisecond),
			baseline.Round(time.Millisecond), added.Round(time.Millisecond), bound, serial,
			float64(serial)/float64(max(added, time.Millisecond)))
		t.Logf("S1 counters: %d objects, %d S3 requests (%.2f per file), %d bytes up and %d down "+
			"on %d logical (%.3f wire ratio)",
			meas.objects, meas.counters.requests, perFile, meas.counters.up, meas.counters.down,
			m.bytes, ratio)

		row := meas.row(m, smokeBacklogScenario, smokeBacklogBudget)
		row.BaselineSeconds = baseline.Seconds()
		recordResult(t, row)

		if counts["shipped"] < m.files {
			t.Fatalf("the backlog run shipped %d of %d files", counts["shipped"], m.files)
		}
		if counts["failed"] != 0 {
			t.Fatalf("the backlog run failed %d files:\n%s", counts["failed"], meas.last.Output)
		}
		if added >= bound {
			t.Errorf("%v of latency on %d files added %v, over the %v bound: "+
				"the run is paying round trips one at a time", smokeRTT, m.files, added, bound)
		}
		if perFile < smokeRequestsPerFileMin || perFile > smokeRequestsPerFileMax {
			t.Errorf("the backlog made %d S3 requests for %d files, %.2f each, outside the "+
				"%.2f to %.2f band", meas.counters.requests, m.files, perFile,
				smokeRequestsPerFileMin, smokeRequestsPerFileMax)
		}
		if ratio > smokeWireRatioMax {
			t.Errorf("the backlog put %d bytes on the wire for %d logical bytes, a ratio of %.3f "+
				"over the %.2f bound", moved, m.bytes, ratio, smokeWireRatioMax)
		}
		if want := m.files + smokeSidecarObjects; meas.objects != want {
			t.Errorf("the backlog left %d objects under %s, want exactly %d "+
				"(%d files and %d sidecars)", meas.objects, m.w.keyRoot, want, m.files,
				smokeSidecarObjects)
		}

		assertNoResidualScratch(t, m.w)
		assertPeakUnderBudget(t, smokeBacklogScenario, meas.peak, smokeBacklogBudget)
	})
	return result
}

// --- S2: the sync after it ---------------------------------------------------

// What a machine with nothing to do costs, measured on the world S1 left behind. The floor is not
// zero (config fetch, heartbeat, project map), so the claim is a ratio.
func smokeSteady(t *testing.T, m *smokeMachine, backlog smokeBacklogResult) {
	t.Run("S2-incremental-no-op-sync", func(t *testing.T) {
		if backlog.best <= 0 || backlog.bytes <= 0 || backlog.versions == nil {
			t.Skipf("S2 measures the sync after S1's backlog and S1 left no baseline behind: "+
				"run %s whole", t.Name())
		}

		// The first no-op run discovers there is nothing to do; the timed ones measure the steady state.
		m.w.mustSync(t)

		meas := smokeRepeat(t, m, warmRuns)
		moved := meas.counters.up + meas.counters.down
		counts := summary(t, meas.last.Output)

		t.Logf("S2 steady state on %d files: reps %s, best %v against the backlog's %v; "+
			"%d bytes against %d, %d S3 requests",
			m.files, durationList(meas.reps), meas.best.Round(time.Millisecond),
			backlog.best.Round(time.Millisecond), moved, backlog.bytes, meas.counters.requests)

		// Recorded before anything is judged: the run worth having numbers for is the one that failed.
		recordResult(t, meas.row(m, smokeSteadyScenario, smokeSteadyBudget))

		if counts["unchanged"] < m.files {
			t.Errorf("the second run called %d files unchanged, want at least %d: "+
				"the pre-filter is opening files it does not need to", counts["unchanged"], m.files)
		}
		if moved*20 >= backlog.bytes {
			t.Errorf("the second run moved %d bytes against the backlog's %d, over a twentieth: "+
				"an unchanged machine is still talking to the store", moved, backlog.bytes)
		}
		if meas.best*3 >= backlog.best {
			t.Errorf("the second run took %v against the backlog's %v, over a third: "+
				"discovery is costing what shipping cost", meas.best, backlog.best)
		}
		for key, after := range m.w.versionCounts(t) {
			if !strings.Contains(key, transcriptPrefix) {
				continue
			}
			if before := backlog.versions[key]; after != before {
				t.Errorf("%s: %d versions after the backlog, %d after the no-op runs; "+
					"an unchanged file was re-shipped", key, before, after)
			}
		}

		// Rewriting the heartbeat and project map is where an unrenamed temp file would show up.
		assertNoResidualScratch(t, m.w)
		assertPeakUnderBudget(t, smokeSteadyScenario, meas.peak, smokeSteadyBudget)
	})
}

// --- the repetitions ---------------------------------------------------------

// Named at the call sites, because a bare boolean there says nothing.
const (
	coldRuns = true
	warmRuns = false
)

// What a run of repetitions came to.
type smokeMeasurement struct {
	reps []time.Duration
	best time.Duration

	// last is the repetition the counters were read around; peak is the highest, rarely the same
	// run, because a spike in any repetition is the finding.
	last childObservation
	peak childObservation

	counters storeCounters
	objects  int
}

// The results row a repeated scenario records; the scenario adds whatever else it measured.
func (meas smokeMeasurement) row(m *smokeMachine, scenario string, budget int64) perfResult {
	return perfResult{
		Scenario:          scenario,
		CorpusFiles:       m.files,
		CorpusBytes:       m.bytes,
		ChildGOMAXPROCS:   m.w.gomaxprocs,
		ShapedRTTMillis:   smokeRTT.Milliseconds(),
		RepSeconds:        repSeconds(meas.reps),
		BestSeconds:       meas.best.Seconds(),
		ProxiedBytesUp:    meas.counters.up,
		ProxiedBytesDown:  meas.counters.down,
		S3Requests:        meas.counters.requests,
		Objects:           meas.objects,
		PeakRSSBytes:      meas.peak.PeakRSS,
		PeakRSSSource:     meas.peak.PeakRSSSource,
		MemoryBudgetBytes: budget,
		CPUSeconds:        meas.last.CPUSeconds,
		ExitStatus:        meas.last.exitStatus(),
	}
}

// cold resets the world before each repetition, making every one a first sync. The counters are read
// around the last repetition only: a scrape settles for a second and every repetition ships the same.
func smokeRepeat(t *testing.T, m *smokeMachine, cold bool) smokeMeasurement {
	t.Helper()
	var meas smokeMeasurement
	sync := func() { meas.last = m.w.mustSync(t) }
	for rep := range smokeReps {
		// The reset comes before the counters are read, so this run's enrollment is already done.
		if cold {
			m.w.reset(t)
		}
		if rep == smokeReps-1 {
			meas.counters = aroundStore(t, sync)
		} else {
			sync()
		}
		meas.reps = append(meas.reps, meas.last.Elapsed)
		if meas.last.PeakRSS > meas.peak.PeakRSS {
			meas.peak = meas.last
		}
	}
	meas.best = slices.Min(meas.reps)
	meas.objects = len(m.w.currentKeys(t))
	return meas
}

// --- the machine -------------------------------------------------------------

// Builds the world and writes the corpus into it.
func stageSmokeMachine(t *testing.T) *smokeMachine {
	t.Helper()
	w := stageWorld(t)
	w.gomaxprocs = smokeGOMAXPROCS

	bytes := stageCorpusFiles(t, w, smokeCorpusFiles)
	t.Logf("smoke corpus: %d files, %d bytes staged, child at GOMAXPROCS=%d",
		smokeCorpusFiles, bytes, w.gomaxprocs)
	return &smokeMachine{w: w, files: smokeCorpusFiles, bytes: bytes}
}

func durationList(ds []time.Duration) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.Round(time.Millisecond).String()
	}
	return strings.Join(parts, " ")
}

func repSeconds(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = d.Seconds()
	}
	return out
}
