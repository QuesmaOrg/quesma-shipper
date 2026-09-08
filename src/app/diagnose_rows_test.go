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
	rows = append(rows, stateRowsFrom(engine.Peek(dir))...)
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
