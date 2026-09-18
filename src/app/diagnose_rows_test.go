package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// TestDiscoveryRowsSeverity pins the severity of every health state, and that NO source state
// is SevFail: a broken source loses that source's data, never the exit code.
func TestDiscoveryRowsSeverity(t *testing.T) {
	src := config.ResolvedSource{Source: sources.Source{ID: "s"}}
	cases := []struct {
		name    string
		d       sources.Discovery
		wantSev Severity
		wantFix string // substring of the first row's fix; empty means no fix expected
	}{
		{"collected", sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK}, SevOK, ""},
		{"collected_bad_sniff",
			sources.Discovery{Health: sources.Collected, Sniff: sources.SniffUnexpectedShape},
			SevWarn, "quesma-shipper preview"},
		{"agent_absent", sources.Discovery{Health: sources.AgentAbsent, Reason: "no root"}, SevDim, ""},
		{"moved", sources.Discovery{Health: sources.RootPresentNoMatch, Reason: "root exists"},
			SevWarn, "moved"},
		{"unreadable", sources.Discovery{Health: sources.MatchPresentUnreadable, Reason: "denied"},
			SevWarn, "permissions"},
	}
	for _, c := range cases {
		rows := discoveryRows(src, c.d)
		if len(rows) == 0 {
			t.Fatalf("%s: no rows", c.name)
		}
		if rows[0].Sev != c.wantSev {
			t.Errorf("%s: severity %v, want %v", c.name, rows[0].Sev, c.wantSev)
		}
		if c.wantFix != "" && !strings.Contains(rows[0].Fix, c.wantFix) {
			t.Errorf("%s: fix %q does not mention %q", c.name, rows[0].Fix, c.wantFix)
		}
		if c.wantFix == "" && rows[0].Sev >= SevWarn && rows[0].Fix == "" {
			t.Errorf("%s: a warning without a fix", c.name)
		}
		for _, row := range rows {
			if row.Sev == SevFail {
				t.Errorf("%s: source states must never be SevFail (row %q)", c.name, row.Label)
			}
		}
	}
}

// Size-cap and unreadable counts each get their own warning with the exact remedy.
func TestDiscoveryRowsLossSubRows(t *testing.T) {
	src := config.ResolvedSource{Source: sources.Source{ID: "claude"}}
	rows := discoveryRows(src, sources.Discovery{
		Health: sources.Collected, Sniff: sources.SniffOK,
		Unreadable: 2, UnreadableReason: "permission denied", UnreadableExample: "/x/y",
		Oversize: []sources.Oversize{{RelPath: "big", Size: 200, Limit: 100}},
	})
	if rows[0].Sev != SevOK {
		t.Fatalf("main row severity %v, want SevOK", rows[0].Sev)
	}
	var fixes []string
	for _, row := range rows[1:] {
		if row.Sev != SevWarn {
			t.Errorf("sub-row %q severity %v, want SevWarn", row.Label, row.Sev)
		}
		fixes = append(fixes, row.Fix)
	}
	all := strings.Join(fixes, "\n")
	for _, want := range []string{"max_file_bytes", "/x/y"} {
		if !strings.Contains(all, want) {
			t.Errorf("sub-row fixes missing %q in:\n%s", want, all)
		}
	}
}

func TestCheckUpdate(t *testing.T) {
	ctx := context.Background()
	noEnv := func(string) string { return "" }
	published := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	fixed := func(latest string, available bool, err error) func(context.Context, packaging.UpdateOptions) (string, time.Time, bool, error) {
		return func(context.Context, packaging.UpdateOptions) (string, time.Time, bool, error) {
			return latest, published, available, err
		}
	}
	boom := func(context.Context, packaging.UpdateOptions) (string, time.Time, bool, error) {
		t.Fatal("a dev build or a disabled check must not touch the network")
		return "", time.Time{}, false, nil
	}

	disabled := func(k string) string {
		if k == NoSelfUpdateEnv {
			return "1"
		}
		return ""
	}
	if got := checkUpdate(ctx, Build{Release: true}, true, disabled, boom); got.State != "disabled" {
		t.Errorf("env off switch: state %q", got.State)
	}
	if got := checkUpdate(ctx, Build{Release: true}, false, noEnv, boom); got.State != "disabled" {
		t.Errorf("autoupdate.enabled off switch: state %q", got.State)
	}
	// A dev build never checks: no TUF, no source repository, no network.
	if got := checkUpdate(ctx, Build{Release: false}, true, noEnv, boom); got.State != "skipped" {
		t.Errorf("dev build: state %q, want skipped", got.State)
	}
	if got := checkUpdate(ctx, Build{Release: true}, true, noEnv, fixed("", false, errors.New("dns"))); got.State != "failed" {
		t.Errorf("transport error: state %q", got.State)
	}
	got := checkUpdate(ctx, Build{Release: true, Version: "1.0.0"}, true, noEnv, fixed("1.1.0", true, nil))
	if got.State != "available" || got.Fix != "quesma-shipper update" {
		t.Errorf("available: %+v must name the command to run", got)
	}
	if !strings.Contains(got.Detail, "2026-08-18") {
		t.Errorf("available: %q must date the release", got.Detail)
	}
	if got := checkUpdate(ctx, Build{Release: true}, true, noEnv, fixed("1.0.0", false, nil)); got.State != "current" {
		t.Errorf("current: state %q", got.State)
	}
}

// Everything status shares with doctor stays out of the exit code.
func TestAdvisoryRowsNeverFail(t *testing.T) {
	dir := t.TempDir()
	var rows []Row
	rows = append(rows, scheduleRows(dir, time.Now())...)
	doc, docErr := engine.Peek(dir)
	rows = append(rows, stateRowsFrom(doc, docErr, "")...)
	rows = append(rows, enrollmentRows(dir, nil, os.ErrNotExist, &config.Effective{}, controlplane.Remote{})...)
	rows = append(rows, enrollmentRows(dir, nil, errors.New("corrupt"), &config.Effective{}, controlplane.Remote{})...)
	for _, row := range rows {
		if row.Sev == SevFail {
			t.Errorf("advisory row %q is SevFail", row.Label)
		}
	}
}

// TestFamilyRows pins the agent-grouped view: one headline per agent, sub-rows for findings.
func TestFamilyRows(t *testing.T) {
	probe := func(id, family string, d sources.Discovery) sourceProbe {
		return sourceProbe{src: config.ResolvedSource{Source: sources.Source{ID: id, Family: family}}, d: d}
	}
	enable := func(pr sourceProbe) sourceProbe { pr.src.Enabled = true; return pr }

	t.Run("healthy multi-source agent is one line", func(t *testing.T) {
		rows, collecting, files := familyRows("Claude Code", []sourceProbe{
			enable(probe("claude-code-transcripts", "claude-code",
				sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK,
					Candidates: make([]sources.Candidate, 1200)})),
			enable(probe("claude-code-context", "claude-code",
				sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK,
					Candidates: make([]sources.Candidate, 34)})),
		}, familyUpload{}, time.Now(), true)
		if !collecting || files != 1234 {
			t.Fatalf("collecting=%v files=%d", collecting, files)
		}
		if len(rows) != 1 {
			t.Fatalf("healthy agent must be one row, got %d: %+v", len(rows), rows)
		}
		if rows[0].Sev != SevOK || rows[0].Label != "Claude Code" {
			t.Errorf("headline: %+v", rows[0])
		}
		for _, want := range []string{"1,234 files", "transcripts", "context"} {
			if !strings.Contains(rows[0].Detail, want) {
				t.Errorf("detail %q missing %q", rows[0].Detail, want)
			}
		}
	})

	t.Run("headline version tag comes from the sniffed store, not an exec", func(t *testing.T) {
		rows, _, _ := familyRows("Claude Code", []sourceProbe{
			enable(probe("claude-code-transcripts", "claude-code",
				sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK,
					AgentVersion: "2.1.245", Candidates: make([]sources.Candidate, 3)})),
		}, familyUpload{}, time.Now(), false)
		if rows[0].Tag != "2.1.245" {
			t.Errorf("headline tag %q, want the sniffed version 2.1.245", rows[0].Tag)
		}
	})

	t.Run("a sibling source with no files is a note, not a finding", func(t *testing.T) {
		probes := []sourceProbe{
			enable(probe("codex-rollouts", "codex",
				sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK})),
			enable(probe("codex-rollouts-compressed", "codex",
				sources.Discovery{Health: sources.RootPresentNoMatch, Reason: "nothing matched"})),
		}
		rows, _, _ := familyRows("Codex", probes, familyUpload{}, time.Now(), false)
		if len(rows) != 1 || rows[0].Sev != SevOK {
			t.Fatalf("default view: headline ✓ and nothing else, got %+v", rows)
		}
		rows, _, _ = familyRows("Codex", probes, familyUpload{}, time.Now(), true)
		if len(rows) != 2 || rows[1].Sev != SevDim || !rows[1].Sub || !strings.Contains(rows[1].Detail, "no rollouts-compressed files yet") {
			t.Fatalf("--all: a dim note under the headline, got %+v", rows)
		}
	})

	t.Run("an agent whose only folder has nothing is a finding that names the folder", func(t *testing.T) {
		src := probe("codex-rollouts", "codex", sources.Discovery{Health: sources.RootPresentNoMatch})
		src.src.Root = "/home/x/.codex/sessions"
		rows, _, _ := familyRows("Codex", []sourceProbe{enable(src)}, familyUpload{}, time.Now(), false)
		if len(rows) != 2 || rows[0].Sev != SevWarn || !rows[1].Sub {
			t.Fatalf("want a ! headline and one Sub finding, got %+v", rows)
		}
		if !strings.Contains(rows[1].Detail, "/home/x/.codex/sessions") || !strings.Contains(rows[1].Detail, "Codex may have changed where it writes") {
			t.Errorf("the finding must name the folder and the likely cause: %q", rows[1].Detail)
		}
		r := &Report{Sections: []Section{{Rows: rows}}}
		if iss, fails := r.Issues(); len(iss)-fails != 1 {
			t.Errorf("one agent with one finding must count as 1 issue, got %d", len(iss)-fails)
		}
	})

	t.Run("disabled source is dim, never a finding", func(t *testing.T) {
		rows, collecting, _ := familyRows("Cursor", []sourceProbe{
			probe("cursor-transcripts", "cursor", sources.Discovery{}),
		}, familyUpload{}, time.Now(), true)
		if collecting || rows[0].Sev != SevDim || !strings.Contains(rows[0].Detail, "disabled by configuration") {
			t.Fatalf("a deliberately disabled agent must headline dim: %+v", rows)
		}
		r := &Report{Sections: []Section{{Rows: rows}}}
		if iss, fails := r.Issues(); fails != 0 || len(iss) != 0 {
			t.Errorf("disabled by configuration must not count: fails=%d issues=%d", fails, len(iss))
		}
	})

	t.Run("absent agent is one dim line", func(t *testing.T) {
		rows, collecting, _ := familyRows("Wire-capture proxy", []sourceProbe{
			enable(probe("wire-proxy-flows", "wire-proxy",
				sources.Discovery{Health: sources.AgentAbsent, Reason: "no root"})),
		}, familyUpload{}, time.Now(), true)
		if collecting || len(rows) != 1 || rows[0].Sev != SevDim {
			t.Fatalf("absence must be one dim row: %+v", rows)
		}
		if !strings.Contains(rows[0].Detail, "not installed") {
			t.Errorf("detail %q should say not installed", rows[0].Detail)
		}
	})
}

// The per-agent upload answer, with failures escalating to a warning.
func TestFamilyUploadRow(t *testing.T) {
	probes := []sourceProbe{{
		src: config.ResolvedSource{Source: sources.Source{ID: "x", Family: "f"}, Enabled: true},
		d:   sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK, Candidates: make([]sources.Candidate, 5)},
	}}
	now := time.Now()

	rows, _, _ := familyRows("F", probes,
		familyUpload{recorded: true, at: now.Add(-9 * time.Minute), shipped: 12, pending: 3}, now, true)
	if len(rows) != 2 || rows[1].Label != "  last upload" || rows[1].Sev != SevDim {
		t.Fatalf("expected a dim last-upload sub-row: %+v", rows)
	}
	for _, want := range []string{"9 min ago", "12 files sent", "3 files changed since"} {
		if !strings.Contains(rows[1].Detail, want) {
			t.Errorf("detail %q missing %q", rows[1].Detail, want)
		}
	}

	rows, _, _ = familyRows("F", probes,
		familyUpload{recorded: true, at: now, failed: 2}, now, false)
	if rows[1].Sev != SevWarn || rows[1].Fix == "" {
		t.Errorf("failed uploads must warn with a fix: %+v", rows[1])
	}
	if !strings.Contains(rows[1].Detail, "nothing new") {
		t.Errorf("zero shipped should read as checked/nothing new: %q", rows[1].Detail)
	}

	rows, _, _ = familyRows("F", probes, familyUpload{}, now, true)
	if len(rows) != 1 {
		t.Errorf("no record and nothing pending should add no sub-row: %+v", rows)
	}
}

func TestClaudeHeadline(t *testing.T) {
	cand := func(rel string) sources.Candidate { return sources.Candidate{RelPath: rel} }
	probes := []sourceProbe{
		{src: config.ResolvedSource{Source: sources.Source{ID: "claude-code-transcripts", Family: "claude-code"}, Enabled: true},
			d: sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK, Candidates: []sources.Candidate{
				cand("projects/alpha/a.jsonl"),
				cand("projects/alpha/b.jsonl"),
				cand("projects/beta/c.jsonl"),
				cand("projects/beta/c.meta.json"), // join metadata, not a session
			}}},
		{src: config.ResolvedSource{Source: sources.Source{ID: "claude-code-settings", Family: "claude-code"}, Enabled: true},
			d: sources.Discovery{Health: sources.Collected, Sniff: sources.SniffOK, Candidates: make([]sources.Candidate, 2)}},
	}
	got := claudeHeadline(probes, true)
	want := "3 sessions in 2 projects, plus settings"
	if got != want {
		t.Errorf("claudeHeadline = %q, want %q", got, want)
	}

	if got := claudeHeadline([]sourceProbe{{src: config.ResolvedSource{Source: sources.Source{ID: "codex-rollouts", Family: "codex"}}}}, true); got != "" {
		t.Errorf("other families must fall back to the generic count, got %q", got)
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 999: "999", 1000: "1,000", 6873: "6,873", 1234567: "1,234,567"}
	for n, want := range cases {
		if got := HumanCount(n); got != want {
			t.Errorf("HumanCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestAccountInspectionIsNeutral(t *testing.T) {
	src := config.ResolvedSource{Source: sources.Source{ID: "codex-account", Family: "codex", Gather: "account"}, Root: t.TempDir(), Enabled: true}
	d, err := (&sources.Accounts{}).Discover(sources.Request{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	for _, verbose := range []bool{false, true} {
		rows, collecting, files := familyRows("Codex", []sourceProbe{{src: src, d: d}}, familyUpload{}, time.Now(), verbose)
		if collecting || files != 0 || len(rows) != 1 || rows[0].Sev != SevDim || rows[0].Detail != "configured; checked during collection" {
			t.Fatalf("unexpected account inspection: %+v collecting=%v files=%d", rows, collecting, files)
		}
	}
	rows := discoveryRows(src, d)
	if len(rows) != 1 || rows[0].Sev != SevDim || rows[0].Detail != d.Reason || rows[0].Fix != "" {
		t.Fatalf("unexpected source inspection: %+v", rows)
	}
}

// Peek skips Open's guard, so doctor must name the mismatch itself rather than wait for a run.
func TestStateRowsNameAnInstallMismatch(t *testing.T) {
	const mine, theirs = "c033b5b2-c3ac-4f39-911f-7ea632b7727c", "85a7e04c-32a4-4bf5-9c80-49c4f9d087bb"
	doc := engine.Document{InstallID: theirs, Entries: map[engine.Key]engine.Fingerprint{}}

	var found *Row
	for _, row := range stateRowsFrom(doc, nil, mine) {
		if row.Sev == SevWarn {
			r := row
			found = &r
		}
	}
	if found == nil {
		t.Fatal("a document written by another install produced no warning")
	}
	if !strings.Contains(found.Detail, theirs) || !strings.Contains(found.Detail, mine) {
		t.Errorf("detail %q must name both installs", found.Detail)
	}
	if !strings.Contains(found.Fix, "state reset") {
		t.Errorf("fix %q must name the command that repairs it", found.Fix)
	}

	// The same document under its own install is ordinary state, not a finding.
	for _, row := range stateRowsFrom(engine.Document{InstallID: mine}, nil, mine) {
		if row.Sev == SevWarn {
			t.Errorf("matching install ids warned: %+v", row)
		}
	}
	// An install whose identity could not be read cannot judge the document either way.
	for _, row := range stateRowsFrom(doc, nil, "") {
		if row.Sev == SevWarn {
			t.Errorf("warned with no identity to compare against: %+v", row)
		}
	}
}

// seedFailures writes a record with a streak and the events behind it, the shape every row test
// below needs.
func seedFailures(t *testing.T, dir string, streak int, events ...formats.FailureEvent) {
	t.Helper()
	rec := formats.FailureRecord{ConsecutiveFailures: streak}
	for _, e := range events {
		rec.Append(e)
	}
	if err := writeFailureRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
}

func event(at time.Time, kind, message string) formats.FailureEvent {
	return formats.FailureEvent{At: at.Format(time.RFC3339), Kind: kind, Message: message}
}

// Nineteen consecutive failed ticks used to leave doctor entirely green: the service row reports
// only that launchd loaded the job, and nothing else read the failure record. This pins the row
// that makes a silently failing install visible, that it carries the reason, and that it says
// nothing once a run succeeds and clears the streak.
func TestFailureRowsReportConsecutiveFailures(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)

	if rows := failureRows(dir, now); rows != nil {
		t.Fatalf("a store with no failure record produced %d rows", len(rows))
	}

	seedFailures(t, dir, 19, event(now.Add(-15*time.Minute), formats.FailureTick,
		"state: document belongs to a different install"))
	rows := failureRows(dir, now)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Sev != SevWarn {
		t.Errorf("severity = %v, want SevWarn", got.Sev)
	}
	if !strings.Contains(got.Detail, "19") {
		t.Errorf("detail %q must count the failed runs", got.Detail)
	}
	if !strings.Contains(got.Fix, "different install") {
		t.Errorf("fix %q must carry the reason the runs failed", got.Fix)
	}

	seedFailures(t, dir, 0)
	if rows := failureRows(dir, now); rows != nil {
		t.Errorf("a healthy install produced %d rows", len(rows))
	}
}

// The streak counts collecting runs, but the log also carries uncounted events: a failed
// self-update, a panic in a one-shot verb. Attributing it to the newest event of ANY kind sent the
// operator to the update channel while the sink was what refused every run.
func TestFailureRowsIgnoreUncountedEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)

	seedFailures(t, dir, 5,
		event(now.Add(-30*time.Minute), formats.FailureTick,
			"the run shipped nothing: all 3 attempted uploads failed: connection refused"),
		event(now.Add(-1*time.Minute), formats.FailureUpdate, "self-update from v1.2.3 did not happen"))

	rows := failureRows(dir, now)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if strings.Contains(rows[0].Fix, "self-update") {
		t.Errorf("fix %q blamed an uncounted event for the streak", rows[0].Fix)
	}
	if !strings.Contains(rows[0].Fix, "connection refused") {
		t.Errorf("fix %q lost the reason the runs actually failed", rows[0].Fix)
	}
	if !strings.Contains(rows[0].Detail, "30 min ago") {
		t.Errorf("detail %q dated the streak from an uncounted event", rows[0].Detail)
	}
}

// Twenty uncounted events can evict every counted one from the bounded log. The count is still
// true, so the row stays; only the reason is gone.
func TestFailureRowsSurviveALogWithNoCountedEvent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	seedFailures(t, dir, 3, event(now, formats.FailureUpdate, "update failed"))

	rows := failureRows(dir, now)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if strings.Contains(rows[0].Fix, "update failed") {
		t.Errorf("fix %q fell back to an uncounted event", rows[0].Fix)
	}
}

// A recorded error carries its remedy in a later paragraph, and the row that owns that remedy
// prints it. This row takes the reason only, so doctor never states one fix twice.
func TestFailureRowsShowTheReasonNotTheRemedy(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	seedFailures(t, dir, 4, event(now, formats.FailureTick,
		"state: document belongs to a different install: it was written by 85a7e04c and this install is c033b5b2\n\n"+
			engine.InstallMismatchRemedy))

	rows := failureRows(dir, now)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if strings.Contains(rows[0].Fix, "state reset") {
		t.Errorf("fix %q repeats the remedy the state row already prints", rows[0].Fix)
	}
	if !strings.Contains(rows[0].Fix, "written by 85a7e04c") {
		t.Errorf("fix %q dropped the reason", rows[0].Fix)
	}
}
