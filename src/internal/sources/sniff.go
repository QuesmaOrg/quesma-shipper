package sources

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sniffJSONL asserts the first non-empty line parses as JSON, and opportunistically reads the producer version out of the head.
func sniffJSONL(head []byte, truncated bool) (SniffResult, string) {
	// A NUL byte settles it: no JSONL store contains one, so this store became binary.
	if bytes.IndexByte(head, 0) >= 0 {
		return SniffUnexpectedShape, ""
	}

	line, terminated := firstLine(head)

	// "No newline" means two things: a truncated head simply ran past the scan budget, a whole file with none is real drift.
	unjudgeable := !terminated && truncated

	if strings.TrimSpace(line) == "" {
		if unjudgeable {
			return SniffOK, ""
		}
		return SniffEmpty, ""
	}

	var rec map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		if unjudgeable {
			return SniffOK, ""
		}
		return SniffUnexpectedShape, ""
	}
	if v := versionFrom(rec); v != "" {
		return SniffOK, v
	}
	// The version rides a header line that is not always the first, so scan on past line one.
	_, rest, _ := bytes.Cut(head, []byte{'\n'})
	return SniffOK, versionFromHead(rest)
}

const maxVersionScanLines = 16

func versionFromHead(head []byte) string {
	scanned := 0
	for line := range bytes.Lines(head) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if scanned++; scanned > maxVersionScanLines {
			break
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal(line, &rec) == nil {
			if v := versionFrom(rec); v != "" {
				return v
			}
		}
	}
	return ""
}

// versionFrom looks for a producer version under the field names the surveyed stores actually use.
func versionFrom(rec map[string]json.RawMessage) string {
	for _, field := range []string{"version", "cli_version"} {
		if raw, ok := rec[field]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return s
			}
		}
	}
	// Codex nests it one level down.
	if raw, ok := rec["payload"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err == nil {
			return versionFrom(nested)
		}
	}
	return ""
}

// firstLine returns the first line and whether a terminator was seen: a line cut off by the budget is not malformed.
func firstLine(head []byte) (string, bool) {
	if line, _, found := bytes.Cut(head, []byte{'\n'}); found {
		return string(bytes.TrimRight(line, "\r")), true
	}
	return string(head), false
}
