package transforms

import (
	"fmt"
	"strings"
)

// Dropped is the one rule id every drop reports.
const Dropped = "dropped"

var droppedSentinel = Sentinel(Dropped)

// CompiledDrops is the compiled baseline of fields whose string value, from dropMinLength up, is
// replaced whole by a sentinel before any detector runs, whatever it holds: they carry encrypted
// reasoning and inline media, which the entropy backstop almost always shreds into undecodable
// fragments. Exact field paths, as in CompiledExemptions.
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
		"pi": {
			"message.content[].thinkingSignature",
			"message.content[].data",
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

// A shorter raw body (a plain URL, "") stays and is scrubbed; the shortest opaque one seen locally is 184.
const dropMinLength = 160
