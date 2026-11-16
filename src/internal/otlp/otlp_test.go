package otlp

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func decodeFixture(t *testing.T, signal Signal, name, contentType string) []Event {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	events, dropped, err := Decode(signal, contentType, raw)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || len(events) == 0 {
		t.Fatalf("%s: %d events, %d dropped", name, len(events), dropped)
	}
	return events
}

// The protobuf fixtures were written by the OpenTelemetry collector's own marshaler from the JSON
// ones, so agreement here checks the hand-written field numbers against an independent encoder.
func TestBothEncodingsDecodeToTheSameLines(t *testing.T) {
	for _, c := range []struct {
		signal Signal
		name   string
	}{{Logs, "claude-code-logs"}, {Metrics, "claude-code-metrics"}, {Logs, "codex-logs"}} {
		fromJSON := decodeFixture(t, c.signal, c.name+".json", "application/json")
		fromProto := decodeFixture(t, c.signal, c.name+".pb", "application/x-protobuf")
		if len(fromJSON) != len(fromProto) {
			t.Fatalf("%s: %d lines from JSON, %d from protobuf", c.name, len(fromJSON), len(fromProto))
		}
		for i := range fromJSON {
			if !bytes.Equal(fromJSON[i].Line, fromProto[i].Line) {
				t.Fatalf("%s line %d differs:\njson  %s\nproto %s", c.name, i, fromJSON[i].Line, fromProto[i].Line)
			}
			if a, b := mustJSON(t, fromJSON[i].Claims), mustJSON(t, fromProto[i].Claims); a != b {
				t.Fatalf("%s line %d claims differ: %s vs %s", c.name, i, a, b)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func find(t *testing.T, events []Event, needle string) map[string]any {
	t.Helper()
	for _, e := range events {
		if strings.Contains(string(e.Line), needle) {
			var line map[string]any
			if err := json.Unmarshal(e.Line, &line); err != nil {
				t.Fatal(err)
			}
			return line
		}
	}
	t.Fatalf("no line contains %s", needle)
	return nil
}

func TestClaudeCodeRequestKeepsItsTokensAndLosesItsIdentity(t *testing.T) {
	events := decodeFixture(t, Logs, "claude-code-logs.json", "application/json")
	line := find(t, events, `"output_tokens":537`)
	attrs := line["attrs"].(map[string]any)
	for key, want := range map[string]float64{"input_tokens": 2, "output_tokens": 537, "cache_read_tokens": 24222, "cache_creation_tokens": 14987} {
		if attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, attrs[key], want)
		}
	}
	if attrs["anthropic.plan"] != "max" || attrs["organization.id"] == nil {
		t.Errorf("the plan and the provider organization should stay in the line: %v", attrs)
	}
	if line["agent"] != "claude-code" || line["signal"] != "log" || line["time_unix_nano"] == "" {
		t.Errorf("unexpected envelope: %v", line)
	}
	for _, e := range events {
		if e.Agent != "claude-code" || e.Claims["user.email"] != "dev@example.com" || e.Claims["anthropic.plan"] != "max" {
			t.Fatalf("agent %q claims %v", e.Agent, e.Claims)
		}
		if strings.Contains(string(e.Line), "dev@example.com") || strings.Contains(string(e.Line), "user.account_uuid") {
			t.Fatalf("identity left in a line: %s", e.Line)
		}
	}
}

func TestCodexCompletionCarriesTokensAndTheHostIsAClaim(t *testing.T) {
	events := decodeFixture(t, Logs, "codex-logs.json", "application/json")
	line := find(t, events, `"input_token_count":"18695"`)
	attrs := line["attrs"].(map[string]any)
	// Codex writes most counts as strings and this one as an integer; lines keep what was sent.
	if attrs["cached_token_count"] != float64(8064) || attrs["output_token_count"] != "208" || attrs["conversation.id"] == nil {
		t.Errorf("unexpected completion: %v", attrs)
	}
	for _, e := range events {
		if e.Agent != "codex" || e.Claims["host.name"] != "devs-laptop.local" {
			t.Fatalf("agent %q claims %v", e.Agent, e.Claims)
		}
	}
}

func TestMetricPointsKeepKindAndValue(t *testing.T) {
	events := decodeFixture(t, Metrics, "claude-code-metrics.json", "application/json")
	line := find(t, events, `"metric":"claude_code.active_time.total"`)
	if line["kind"] != "sum" || line["temporality"] != "delta" || line["unit"] != "s" || line["value"] == nil {
		t.Errorf("unexpected point: %v", line)
	}
}

func TestUnknownServicesAreDroppedAndBadInputIsRefused(t *testing.T) {
	body := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},
		"scopeLogs":[{"scope":{"name":"x"},"logRecords":[{"timeUnixNano":"1"}]}]}]}`
	events, dropped, err := Decode(Logs, "application/json; charset=utf-8", []byte(body))
	if err != nil || len(events) != 0 || dropped != 1 {
		t.Fatalf("events %d dropped %d err %v", len(events), dropped, err)
	}
	if _, _, err := Decode(Logs, "text/plain", nil); err == nil {
		t.Error("an unknown content type was accepted")
	}
	if _, _, err := Decode(Logs, "application/x-protobuf", []byte{0xff, 0xff}); err == nil {
		t.Error("malformed protobuf was accepted")
	}
	if _, _, err := Decode(Logs, "application/json", []byte("{")); err == nil {
		t.Error("malformed JSON was accepted")
	}
}
