package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func report() formats.Report {
	return formats.Report{
		StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		Sources: []formats.SourceOutcome{
			{
				SourceID: "claude-code-transcripts", Family: "claude-code",
				Health: sources.Collected, Sniff: sources.SniffOK, AgentVersion: "2.1.220",
				Files: []formats.FileOutcome{
					{Decision: auditlog.DecisionShipped, Density: 0.02,
						NativePath: "/Users/__USER__/.claude/projects/p/a.jsonl",
						ObjectKey:  "v1/organization=default/install=x/mirror/source=s/abc.age"},
					{Decision: auditlog.DecisionUnchanged},
				},
			},
			{
				SourceID: "cursor-transcripts", Family: "cursor",
				Health: sources.RootPresentNoMatch,
				Reason: "root exists but no file matched",
			},
			{SourceID: "codex-rollouts", Family: "codex", Health: sources.AgentAbsent, Reason: "not installed"},
		},
	}
}

// The heartbeat carries no transcript bytes: it answers "is this source still findable", never
// "what did you send".
func TestHeartbeatCarriesNoContentOrUploadState(t *testing.T) {
	hb := engine.Build(engine.Input{
		OrganizationID: "default", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		ClientVersion: "0.1.0", ConfigVersion: 1,
		Report: report(), Now: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
	})
	body, err := hb.Encode()
	if err != nil {
		t.Fatal(err)
	}

	// No object keys, cursor offsets or paths, which would make it upload tracking. Matched against
	// field NAMES: "cursor" alone also matches the legitimate source id "cursor-transcripts".
	for _, forbidden := range []string{
		"abc.age", `"object_key"`, `"cursor_before"`, `"cursor_after"`, `"offset"`,
		`"native_path"`, ".jsonl", "/Users/", "projects/",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("heartbeat contains %q — it must not be upload tracking:\n%s", forbidden, body)
		}
	}

	// What it MUST carry.
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"install_id", "client_version", "config_version", "sources"} {
		if _, ok := parsed[want]; !ok {
			t.Errorf("heartbeat missing %q", want)
		}
	}
}

// The two states that look identical from a distance must stay distinct in the heartbeat.
func TestHeartbeatKeepsAbsentAndNoMatchDistinct(t *testing.T) {
	hb := engine.Build(engine.Input{Report: report(), Now: time.Now()})

	states := map[string]string{}
	for _, s := range hb.Sources {
		states[s.SourceID] = s.State
	}
	if states["codex-rollouts"] != string(sources.AgentAbsent) {
		t.Errorf("codex state %q", states["codex-rollouts"])
	}
	if states["cursor-transcripts"] != string(sources.RootPresentNoMatch) {
		t.Errorf("cursor state %q", states["cursor-transcripts"])
	}
}

func TestHeartbeatRecordsPerSourceCounts(t *testing.T) {
	hb := engine.Build(engine.Input{Report: report(), Now: time.Now()})
	for _, s := range hb.Sources {
		if s.SourceID != "claude-code-transcripts" {
			continue
		}
		if s.Shipped != 1 || s.Unchanged != 1 {
			t.Errorf("counts: shipped %d unchanged %d", s.Shipped, s.Unchanged)
		}
		if s.RedactionDensity <= 0 {
			t.Error("redaction density should be recorded: it is the rule-drift alarm")
		}
		if s.AgentVersion != "2.1.220" {
			t.Errorf("agent version %q", s.AgentVersion)
		}
		return
	}
	t.Error("source not found in heartbeat")
}
