package cli

import (
	"bytes"
	"github.com/QuesmaOrg/quesma-shipper/app"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// Unchanged files are silent, so a steady-state sync prints no progress at all.
func TestProgressLineIsSilentOnUnchanged(t *testing.T) {
	got := progressLine("claude-code", 3, 10, formats.FileOutcome{
		RelPath:  "projects/p/a.jsonl",
		Decision: formats.DecisionUnchanged,
	})
	if got != "" {
		t.Fatalf("unchanged file rendered %q", got)
	}
}

func TestProgressLineRendersShippedWithCounterAndSizes(t *testing.T) {
	got := progressLine("claude-code", 3, 10, formats.FileOutcome{
		RelPath:  "projects/p/a.jsonl",
		Decision: formats.DecisionShipped,
		BytesIn:  465_000,
		BytesOut: 120_000,
	})
	for _, want := range []string{"[3/10]", "claude-code", "projects/p/a.jsonl", "shipped", "454.1 KB in", "117.2 KB sealed"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q missing %q", got, want)
		}
	}
}

// A file that did not ship must say why: "skipped, fine" versus "skipped, go look".
func TestProgressLineCarriesTheReason(t *testing.T) {
	got := progressLine("codex", 1, 4, formats.FileOutcome{
		RelPath:  "sessions/big.jsonl",
		Decision: formats.DecisionSkipped,
		Reason:   "over the 100000000 byte limit for this source",
	})
	if !strings.Contains(got, "skipped: over the 100000000 byte limit") {
		t.Errorf("line %q missing the skip reason", got)
	}
}

// The e2e harness parses these lines, so the shape is a contract: counter, source id, path,
// decision, and no newline of its own.
func TestProgressLineIsOneLineWithNoTerminator(t *testing.T) {
	got := progressLine("claude-code", 1, 1, formats.FileOutcome{
		RelPath: "a.jsonl", Decision: formats.DecisionShipped, BytesIn: 10, BytesOut: 4,
	})
	if strings.Contains(got, "\n") {
		t.Errorf("the line terminates itself: %q", got)
	}
	if want := "[1/1] claude-code  a.jsonl  shipped (10 B in, 4 B sealed)"; got != want {
		t.Errorf("the parsed format moved:\n got %q\nwant %q", got, want)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{465_000, "454.1 KB"},
		{12_165_120, "11.6 MB"},
	}
	for _, c := range cases {
		if got := app.HumanBytes(c.n); got != c.want {
			t.Errorf("app.HumanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// The oversize line has to say WHICH file and HOW BIG: a pathological file and ordinary work
// differ by three orders of magnitude.
func TestTheOversizeLineNamesTheFileAndItsSize(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{
		Sources: []formats.SourceOutcome{{
			SourceID:        "codex-rollouts",
			Health:          formats.Collected,
			Oversize:        1,
			OversizeLargest: 412 << 20,
			OversizeLimit:   256 << 20,
			OversizeExample: "sessions/2026/08/01/rollout-2026-08-01-abc.jsonl",
		}},
	}, false)

	out := buf.String()
	for _, want := range []string{
		"codex-rollouts",
		"1 file over the",              // singular, because there is one
		"256.0 MB",                     // the cap it exceeded
		"412.0 MB",                     // how big it actually is
		"rollout-2026-08-01-abc.jsonl", // and which one
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, out)
		}
	}
	// The cap is per source and configurable, so a reader who wants the file has somewhere to go.
	if !strings.Contains(out, "max_file_bytes") {
		t.Errorf("the line does not say how to change the outcome:\n%s", out)
	}
	if strings.Contains(out, "they will not be") {
		t.Errorf("the old unactionable wording is back:\n%s", out)
	}
}

// Only a truncated one-shot sync advises a drain, and only when it left a backlog.
func TestATruncatedSyncSaysHowToFlushTheRest(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{Truncated: true, Shipped: 64}, true)
	out := buf.String()
	if !strings.Contains(out, "max_files_per_run reached") {
		t.Errorf("the truncation note is gone:\n%s", out)
	}
	if !strings.Contains(out, "run --once --drain") {
		t.Errorf("a truncated sync does not say how to flush the rest:\n%s", out)
	}
}

func TestTheDaemonIsNotToldToDrain(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{Truncated: true, Shipped: 64}, false)
	if strings.Contains(buf.String(), "--drain") {
		t.Errorf("the daemon re-ticks on its own; advising a manual drain:\n%s", buf.String())
	}
}

// Truncation on FAILURES has no backlog worth chasing.
func TestATruncatedRunThatShippedNothingIsNotToldToDrain(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{Truncated: true, Failed: 64}, true)
	if strings.Contains(buf.String(), "--drain") {
		t.Errorf("a run of pure failures was told to drain:\n%s", buf.String())
	}
}

// The line the counters cannot give: how much moved, how long it took, how fast that was.
func TestTheStatsLineReportsBytesAndThroughput(t *testing.T) {
	start := time.Date(2026, 8, 6, 11, 2, 4, 0, time.UTC)
	got := statsLine(formats.Report{
		StartedAt:       start,
		FinishedAt:      start.Add(3400 * time.Millisecond),
		BytesRead:       5_452_595,
		BytesSealed:     1_468_006,
		MedianFileBytes: 215_040,
	})
	want := "  sent  1.4 MB sealed of 5.2 MB read in 3.4s  (1.5 MB/s, median file 210.0 KB)"
	if got != want {
		t.Errorf("stats line:\n got %q\nwant %q", got, want)
	}
	// A sentence, not a row: a tab would column-align it against differently shaped blocks.
	if strings.Contains(got, "\t") {
		t.Errorf("the stats line carries a tab: %q", got)
	}
}

// A steady-state sync read nothing, and "0 B in 0s" on every tick forever is noise.
func TestTheStatsLineIsAbsentWhenNothingWasRead(t *testing.T) {
	start := time.Date(2026, 8, 6, 11, 2, 4, 0, time.UTC)
	if got := statsLine(formats.Report{StartedAt: start, FinishedAt: start.Add(time.Second)}); got != "" {
		t.Errorf("a run that read nothing printed %q", got)
	}
}

// A run whose clock never closed has no denominator, and a rate over one is worse than none.
func TestTheStatsLineIsAbsentWithoutAFinishTime(t *testing.T) {
	if got := statsLine(formats.Report{BytesRead: 1 << 20, BytesSealed: 1 << 19}); got != "" {
		t.Errorf("a run with no finish time printed %q", got)
	}
}

// A paused run says so instead of a summary; no stats line may imply otherwise.
func TestAPausedRunHasNoStatsLine(t *testing.T) {
	var buf bytes.Buffer
	start := time.Date(2026, 8, 6, 11, 2, 4, 0, time.UTC)
	printRunSummary(&buf, formats.Report{
		Paused: true, PauseReason: "by hand",
		StartedAt: start, FinishedAt: start.Add(time.Second), BytesRead: 1 << 20,
	}, false)
	if strings.Contains(buf.String(), "sealed of") {
		t.Errorf("a paused run reported throughput:\n%s", buf.String())
	}
}

// A run inside the clock's resolution keeps its median; the rate goes, since it would be invented.
func TestAZeroDurationRunDropsTheRateAndKeepsTheMedian(t *testing.T) {
	at := time.Date(2026, 8, 6, 11, 2, 4, 0, time.UTC)
	got := statsLine(formats.Report{
		StartedAt: at, FinishedAt: at,
		BytesRead: 4096, BytesSealed: 2048, MedianFileBytes: 4096,
	})
	if strings.Contains(got, "/s") {
		t.Errorf("a zero-duration run reported a rate: %q", got)
	}
	if !strings.Contains(got, "median file 4.0 KB") {
		t.Errorf("the median went with the rate: %q", got)
	}
	if !strings.Contains(got, "in 0ms") {
		t.Errorf("the elapsed time is missing: %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{850 * time.Millisecond, "850ms"},
		{3400 * time.Millisecond, "3.4s"},
		{59500 * time.Millisecond, "59.5s"},
		{200 * time.Second, "3m20s"},
		{65 * time.Minute, "1h05m"},
	}
	for _, c := range cases {
		if got := app.HumanDuration(c.d); got != c.want {
			t.Errorf("app.HumanDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Alarm notes print once, not once per heading, and an informational note about a conversation
// that shipped must not sit under the "DB-side fields lost" banner.
func TestEnrichNotesPrintOnceAndInfosAreNotReportedAsLoss(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{
		Sources: []formats.SourceOutcome{{
			SourceID:       "cursor-transcripts",
			Health:         formats.Collected,
			EnrichMismatch: 1,
			EnrichErrors:   1,
			EnrichNotes:    []string{"conv-a: 2 transcript events did not align with the store"},
			EnrichInfos:    []string{"conv-b: 3 events carried native-only in the derived object"},
		}},
	}, false)
	out := buf.String()

	if got := strings.Count(out, "did not align"); got != 1 {
		t.Errorf("the alarm note prints %d times, want once:\n%s", got, out)
	}
	if !strings.Contains(out, "1 files sent without their database details") || !strings.Contains(out, "enrich errors ×1") {
		t.Fatalf("an alarm heading is missing:\n%s", out)
	}
	if !strings.Contains(out, "carried native-only") {
		t.Fatalf("the informational note is gone entirely:\n%s", out)
	}
	// The info line names itself as shipped; it must not read as part of the loss block.
	if !strings.Contains(out, "enrich note (shipped)") {
		t.Errorf("the informational note has no shipped label:\n%s", out)
	}
	if banner := strings.Index(out, "ENRICH MISMATCH"); banner >= 0 {
		alarmBlock := out[banner:strings.Index(out, "carried native-only")]
		if strings.Contains(alarmBlock, "native-only") {
			t.Errorf("the info note sits inside the alarm block:\n%s", out)
		}
	}
}

// A source whose enrichment raised no alarm prints its infos and nothing else.
func TestInfosAloneRaiseNoBanner(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{
		Sources: []formats.SourceOutcome{{
			SourceID:    "cursor-transcripts",
			Health:      formats.Collected,
			Enriched:    2,
			EnrichInfos: []string{"conv-b: 1 event carried native-only in the derived object"},
		}},
	}, false)
	out := buf.String()
	if strings.Contains(out, "ENRICH MISMATCH") || strings.Contains(out, "enrich errors") {
		t.Errorf("an info-only run printed an alarm:\n%s", out)
	}
	if !strings.Contains(out, "carried native-only") {
		t.Errorf("the info line is missing:\n%s", out)
	}
}

func TestTheOversizeLinePluralises(t *testing.T) {
	var buf bytes.Buffer
	printRunSummary(&buf, formats.Report{
		Sources: []formats.SourceOutcome{{
			SourceID: "claude-code-transcripts", Health: formats.Collected,
			Oversize: 3, OversizeLargest: 900 << 20, OversizeLimit: 256 << 20,
			OversizeExample: "projects/x/big.jsonl",
		}},
	}, false)
	if !strings.Contains(buf.String(), "3 files over the") {
		t.Errorf("plural form missing:\n%s", buf.String())
	}
}
