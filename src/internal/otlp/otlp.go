// Package otlp turns an OTLP/HTTP export from Claude Code or Codex into archive lines: one JSON
// object per log record or metric point. Who sent it is lifted out of every line into Claims, so
// the scrubber's PII rules cannot erase the only name this path carries.
package otlp

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Signal string

const (
	Logs    Signal = "logs"
	Metrics Signal = "metrics"
)

// LineVersion is the "v" every line carries; readers switch on it.
const LineVersion = 1

type Event struct {
	// Agent is "claude-code" or "codex".
	Agent  string
	Claims map[string]any
	Line   []byte

	// Session says the record names a session or conversation, so it is worth keeping unattributed.
	Session bool
}

// claimKeys are what a client says about who it is. They are claims: nothing here proves them.
// The personal ones are removed from the line; the plan and the provider's organization stay, as
// they name no person and a reader prices a call by them.
var claimKeys = map[string]bool{
	"user.email": true, "user.id": true, "user.account_uuid": true, "user.account_id": true, "host.name": true,
	"organization.id": false, "anthropic.plan": false,
}

// record is one log record or metric point as both wire encodings produce it.
type record struct {
	resource map[string]any
	scope    string
	fields   map[string]any
	attrs    map[string]any
}

// Decode reads one export request. Records from a service that is neither agent are dropped and counted.
func Decode(signal Signal, contentType string, body []byte) (events []Event, dropped int, err error) {
	var recs []record
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(mediaType)) {
	case "application/json":
		recs, err = decodeJSON(signal, body)
	case "application/x-protobuf":
		recs, err = decodeProto(signal, body)
	default:
		return nil, 0, fmt.Errorf("otlp: unsupported content type %q", contentType)
	}
	if err != nil {
		return nil, 0, err
	}
	for _, r := range recs {
		agent := agentOf(r)
		if agent == "" {
			dropped++
			continue
		}
		claims := map[string]any{}
		for k, personal := range claimKeys {
			for _, m := range []map[string]any{r.attrs, r.resource} {
				if v, ok := m[k]; ok {
					claims[k] = v
					if personal {
						delete(m, k)
					}
				}
			}
		}
		line := r.fields
		line["v"] = LineVersion
		line["agent"] = agent
		line["scope"] = r.scope
		line["resource"] = r.resource
		line["attrs"] = r.attrs
		raw, err := json.Marshal(line)
		if err != nil {
			return nil, 0, err
		}
		_, session := r.attrs["session.id"]
		_, conversation := r.attrs["conversation.id"]
		events = append(events, Event{Agent: agent, Claims: claims, Line: raw, Session: session || conversation})
	}
	return events, dropped, nil
}

func agentOf(r record) string {
	service, _ := r.resource["service.name"].(string)
	switch {
	case strings.HasPrefix(service, "claude-code"), strings.HasPrefix(r.scope, "com.anthropic.claude_code"):
		return "claude-code"
	case strings.HasPrefix(service, "codex"), strings.HasPrefix(r.scope, "codex"):
		return "codex"
	}
	return ""
}

func temporality(n int64) string {
	switch n {
	case 1:
		return "delta"
	case 2:
		return "cumulative"
	}
	return ""
}
