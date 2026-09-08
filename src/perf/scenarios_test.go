//go:build perf

// The measurements themselves. No t.Parallel anywhere, ever: the package shares one proxy and
// one protocol peer, and a toxic is global to it. Every bound is relative (a multiple of the
// injected RTT, or a ratio between two runs), never an absolute time.
package perf

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A plausible cross-region hop, far enough above loopback noise to not be an artifact.
const shapedRTT = 50 * time.Millisecond

// Selects only what came off the machine; the heartbeat and project map change every run by design.
const transcriptPrefix = "mirror/source=claude-code-transcripts/"

// The claim the upload pool exists for: a backlog against a store 50ms away must not cost one
// round trip per file. Structure is compared first, since a "faster" run that shipped fewer files
// is not faster; each side is the minimum of two samples, because noise only ever adds.
func TestBacklogFirstSyncOverlapsRoundTrips(t *testing.T) {
	base := stageWorld(t)
	files := stageCorpus(t, base)
	baseOut, baseElapsed := base.runSync(t)
	baseCounts := summary(t, baseOut)
	baseElapsed = min(baseElapsed, timingRun(t, files))

	withLatency(t, shapedRTT, 0)

	shaped := stageWorld(t)
	if got := stageCorpus(t, shaped); got != files {
		t.Fatalf("the two worlds got different corpora: %d files and %d", files, got)
	}
	shapedOut, shapedElapsed := shaped.runSync(t)
	shapedCounts := summary(t, shapedOut)
	shapedElapsed = min(shapedElapsed, timingRun(t, files))

	assertSameKeys(t, base.currentKeys(t), shaped.currentKeys(t))
	for _, k := range []string{"shipped", "unchanged", "skipped", "parked", "failed"} {
		if baseCounts[k] != shapedCounts[k] {
			t.Errorf("%s: %d unshaped, %d shaped; the two runs did different work",
				k, baseCounts[k], shapedCounts[k])
		}
	}
	if baseCounts["shipped"] < files {
		t.Fatalf("the backlog run shipped %d of %d files", baseCounts["shipped"], files)
	}

	added := shapedElapsed - baseElapsed
	// One round trip per file over half the compute pool: slack for a loaded machine, still far
	// under the serial cost. The logged overlap ratio is the sharper statement.
	bound := time.Duration(files) * shapedRTT / (childGOMAXPROCS / 2)

	serial := time.Duration(files) * shapedRTT
	t.Logf("backlog %d files: unshaped %v, shaped at %v rtt %v, added %v (bound %v, overlap %.1fx)",
		files, baseElapsed.Round(time.Millisecond), shapedRTT,
		shapedElapsed.Round(time.Millisecond), added.Round(time.Millisecond), bound,
		float64(serial)/float64(max(added, time.Millisecond)))

	if added >= bound {
		t.Errorf("%v of latency on %d files added %v, over the %v bound: "+
			"the run is paying round trips one at a time", shapedRTT, files, added, bound)
	}
}

// A machine whose backlog is already shipped must cost almost nothing, however far the store is.
// The floor is not zero (config fetch, heartbeat, project map), so the claim is a ratio.
func TestSteadyStateSyncMakesAlmostNoNetworkCalls(t *testing.T) {
	withLatency(t, shapedRTT, 0)

	w := stageWorld(t)
	files := stageCorpus(t, w)

	before := proxiedBytes(t)
	backlogOut, backlogElapsed := w.runSync(t)
	backlogBytes := proxiedBytes(t) - before
	if got := summary(t, backlogOut)["shipped"]; got < files {
		t.Fatalf("the backlog run shipped %d of %d files", got, files)
	}
	versionsAfterBacklog := w.versionCounts(t)

	steadyOut, steadyElapsed := w.runSync(t)
	steadyBytes := proxiedBytes(t) - before - backlogBytes
	steadyCounts := summary(t, steadyOut)

	t.Logf("steady state: backlog %v / %d bytes, second run %v / %d bytes",
		backlogElapsed.Round(time.Millisecond), backlogBytes,
		steadyElapsed.Round(time.Millisecond), steadyBytes)

	if steadyCounts["unchanged"] < files {
		t.Errorf("the second run called %d files unchanged, want at least %d: "+
			"the pre-filter is opening files it does not need to",
			steadyCounts["unchanged"], files)
	}
	if steadyBytes*20 >= backlogBytes {
		t.Errorf("the second run moved %d bytes against the backlog's %d, over a twentieth: "+
			"an unchanged machine is still talking to the store", steadyBytes, backlogBytes)
	}
	if steadyElapsed*3 >= backlogElapsed {
		t.Errorf("the second run took %v against the backlog's %v, over a third: "+
			"discovery is costing what shipping cost", steadyElapsed, backlogElapsed)
	}

	for key, after := range w.versionCounts(t) {
		if !strings.Contains(key, transcriptPrefix) {
			continue
		}
		if before := versionsAfterBacklog[key]; after != before {
			t.Errorf("%s: %d versions after the backlog, %d after the second run; "+
				"an unchanged file was re-shipped", key, before, after)
		}
	}
}

// Walks the bound up the latency scale: one rtt could be a coincidence, the curve is the evidence.
// Skipped under -short: it is the whole tier again, once per row.
func TestLatencySensitivity(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the sensitivity table runs a full backlog per row")
	}

	base := stageWorld(t)
	files := stageCorpus(t, base)
	_, baseElapsed := base.runSync(t)
	baseElapsed = min(baseElapsed, timingRun(t, files))
	t.Logf("unshaped baseline: %v for %d files", baseElapsed.Round(time.Millisecond), files)

	for _, rtt := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond} {
		t.Run(fmt.Sprintf("rtt=%v", rtt), func(t *testing.T) {
			withLatency(t, rtt, 0)

			w := stageWorld(t)
			stageCorpus(t, w)
			out, elapsed := w.runSync(t)
			if got := summary(t, out)["shipped"]; got < files {
				t.Fatalf("shipped %d of %d files", got, files)
			}
			elapsed = min(elapsed, timingRun(t, files))

			added := elapsed - baseElapsed
			bound := time.Duration(files) * rtt / (childGOMAXPROCS / 2)
			t.Logf("rtt %v: %v total, %v added, bound %v, %.1fx overlap on a serial %v",
				rtt, elapsed.Round(time.Millisecond), added.Round(time.Millisecond), bound,
				float64(time.Duration(files)*rtt)/float64(max(added, time.Millisecond)),
				time.Duration(files)*rtt)

			if added >= bound {
				t.Errorf("%v of latency added %v, over the %v bound", rtt, added, bound)
			}
		})
	}
}

// The second sample of a measurement, so no bound ever rests on a single wall clock.
func timingRun(t *testing.T, files int) time.Duration {
	t.Helper()
	w := stageWorld(t)
	if got := stageCorpus(t, w); got != files {
		t.Fatalf("the timing world got a different corpus: %d files and %d", files, got)
	}
	out, elapsed := w.runSync(t)
	if got := summary(t, out)["shipped"]; got < files {
		t.Fatalf("the timing run shipped %d of %d files", got, files)
	}
	return elapsed
}

// Two runs are comparable only if they landed the same objects; the keys are install-relative.
func assertSameKeys(t *testing.T, a, b []string) {
	t.Helper()
	slices.Sort(a)
	slices.Sort(b)
	if len(a) != len(b) {
		t.Fatalf("the two runs landed %d and %d keys", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("the two runs disagree at key %d: %q and %q", i, a[i], b[i])
		}
	}
}
