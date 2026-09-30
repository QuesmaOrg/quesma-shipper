package transforms

import (
	"bytes"
	"fmt"
	"strings"
)

// Dropped is the one rule id every drop reports.
const Dropped = "dropped"

var droppedSentinel = Sentinel(Dropped)

// CompiledDrops is the compiled baseline of opaque payloads replaced whole by a sentinel before
// any detector runs: encrypted reasoning and inline base64 media, which the entropy backstop
// almost always shreds into undecodable fragments. Exact field paths, as in CompiledExemptions.
func CompiledDrops() map[string][]string {
	return map[string][]string{
		"claude-code": {
			// thinking.signature and redacted_thinking.data.
			"message.content[].signature",
			"message.content[].data",
			// Image and document blocks; the Read tool stores its file twice.
			"message.content[].source.data",
			"message.content[].content[].source.data",
			"attachment.prompt[].source.data",
			"toolUseResult.file.base64",
		},
		"codex": {
			// Fernet tokens, including their copies in compacted history.
			"payload.encrypted_content",
			"payload.content[].encrypted_content",
			"payload.replacement_history[].encrypted_content",
			"payload.replacement_history[].content[].encrypted_content",
			"payload.guardian_history[].encrypted_content",
			"payload.guardian_history[].content[].encrypted_content",
			"payload.output[].encrypted_content",
			"payload.replacement_history[].output[].encrypted_content",
			"payload.guardian_history[].output[].encrypted_content",
			"payload.item.output[].encrypted_content",
			// Image and audio data URLs, image generation results and MCP image blocks.
			"payload.content[].image_url",
			"payload.output[].image_url",
			"payload.replacement_history[].content[].image_url",
			"payload.replacement_history[].output[].image_url",
			"payload.guardian_history[].content[].image_url",
			"payload.guardian_history[].output[].image_url",
			"payload.images[]",
			"payload.item.content[].image_url",
			"payload.item.result",
			"payload.item.result.content[].data",
			"payload.item.result._meta.codex/toolSurface.screenshot.url",
			"payload.result",
			"payload.result.Ok.content[].data",
			"payload.item.output[].image_url",
			"payload.content[].audio_url",
			"payload.output[].audio_url",
			"payload.replacement_history[].content[].audio_url",
			"payload.replacement_history[].output[].audio_url",
			"payload.guardian_history[].content[].audio_url",
			"payload.guardian_history[].output[].audio_url",
			"payload.item.content[].audio_url",
			"payload.item.output[].audio_url",
			// A dynamic tool call item serializes its content items in camelCase.
			"payload.item.content_items[].imageUrl",
			"payload.item.content_items[].audioUrl",
		},
	}
}

func compileDrops(spec map[string][]string) (map[string]map[FieldPath]bool, error) {
	out := make(map[string]map[FieldPath]bool, len(spec))
	for family, paths := range spec {
		if family == "*" || family == "" {
			return nil, fmt.Errorf("scrub: drops need a source family, got %q", family)
		}
		set := make(map[FieldPath]bool, len(paths))
		for _, path := range paths {
			if path == "" {
				return nil, fmt.Errorf("scrub: drop %s needs a field path", family)
			}
			if strings.Contains(path, embeddedSuffix) {
				return nil, fmt.Errorf("scrub: drop %s %q cannot apply inside an embedded JSON document", family, path)
			}
			if set[FieldPath(path)] {
				return nil, fmt.Errorf("scrub: drop %s %q is listed twice", family, path)
			}
			set[FieldPath(path)] = true
		}
		out[family] = set
	}
	return out, nil
}

// Above a hex SHA-512 (128), below the shortest opaque value seen locally (184).
const dropMinLength = 160

// droppable reports whether the raw JSON string body is base64 or a base64 data URL.
func droppable(body []byte) bool {
	if rest, ok := bytes.CutPrefix(body, []byte("data:")); ok {
		const marker = ";base64,"
		i := bytes.Index(rest[:min(len(rest), 128)], []byte(marker))
		if i < 0 || !mediaTypeShaped(rest[:i]) {
			return false
		}
		body = rest[i+len(marker):]
	}
	return base64Blob(body)
}

// base64Blob is the decoders' acceptance without decoding; a backslash fails it, so raw equals decoded.
func base64Blob(v []byte) bool {
	n := len(v)
	if n < dropMinLength {
		return false
	}
	data := bytes.TrimRight(v, "=")
	pad := n - len(data)
	if pad > 2 || pad > 0 && n%4 != 0 || len(data)%4 == 1 {
		return false
	}
	var seen uint8
	for _, c := range data {
		class := base64Class[c]
		if class == 0 {
			return false
		}
		seen |= class
	}
	return seen&(base64Std|base64URL) != base64Std|base64URL
}

const (
	base64Common = 1 << iota
	base64Std
	base64URL
)

// base64Class is a table because the loop runs over every byte of every image.
var base64Class = func() (t [256]uint8) {
	for c := range 256 {
		switch b := byte(c); {
		case isAlnumByte(b):
			t[c] = base64Common
		case b == '+', b == '/':
			t[c] = base64Std
		case b == '-', b == '_':
			t[c] = base64URL
		}
	}
	return t
}()

func mediaTypeShaped(t []byte) bool {
	for _, c := range t {
		if !isAlnumByte(c) && strings.IndexByte("/+.-_=;", c) < 0 {
			return false
		}
	}
	return true
}
