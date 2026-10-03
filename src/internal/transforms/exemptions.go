package transforms

// CompiledExemptions is the compiled baseline: an entry must be an exact field path naming
// a field that structurally cannot carry a user secret, or "<path>.*" for the keys of a map
// keyed by ids. Detector-scoped, so the pattern packs still scan these fields and keys. The
// copy is fresh, since callers merge served additions in.
func CompiledExemptions() map[string][]string {
	return map[string][]string{
		"claude-code": {
			// The record spine: redact any of these and the DAG dies.
			"uuid", "parentUuid", "logicalParentUuid", "sessionId", "agentId",
			"message.id", "requestId", "promptId", "interruptedMessageId",
			// The session slug ("sleepy-mochi") is long and mixed enough to trip the
			// backstop; path-user still rewrites a username inside it.
			"slug",
			// The spawn-tree and tool-call joins: the same id as the parent transcript,
			// the subagent .meta.json, the tool_result blocks and the server-tool
			// variant each spell it.
			"message.content[].id",
			"message.content[].tool_use_id",
			"toolUseResult.tool_use_id",
			"toolUseResult.results[].tool_use_id",
			"toolUseId",
			"sourceToolUseID",
			"attachment.toolUseID",
			// A filesystem path by construction, and the source of the per-repository
			// dimension downstream (repo is its basename).
			"cwd",
			// Maps keyed by the tool_use id (toolu_…): the key is the join to the tool_use block.
			"wireToolInputs.*", "wireIngestContext.*",
		},
		"codex": {
			// The rollout's tool-call join id (call_…): codex's tool_use_id.
			"payload.call_id",
			// A patch's changed files, keyed by path: a hash-named directory must not tear the path.
			"payload.item.changes.*",
		},
		"cursor": {
			"composerId", "bubbleId", "checkpointId", "requestId",
			// A hex-encoded image the backstop would destroy wholesale: a declared
			// opaque payload, keyed on the nested path.
			"content[].image.hex",
		},
		"copilot": {
			// The event spine and the tool-call and model-call joins of the CLI and VS Code
			// transcripts (call_… and toolu_… ids trip the backstop).
			"id", "parentId",
			"data.toolCallId", "data.toolRequests[].toolCallId", "data.parentToolCallId",
			"data.permissionRequest.toolCallId", "data.promptRequest.toolCallId",
			"data.apiCallId", "data.message.apiCallId", "data.messages[].apiCallId",
			"data.modelCall.api_id", "data.responseChunk.id",
			"data.toolTelemetry.properties.hashedUrl",
			"data.context.cwd",
			// Provider-encrypted reasoning: opaque ciphertext the backstop would shred.
			"data.encryptedContent", "data.reasoningOpaque",
			"data.reasoningBlocks.blocks[].encrypted_content", "data.reasoningBlocks.blocks[].id",
			// VS Code's chatSessions log puts one value at v under three paths: the snapshot
			// (v.requests[]), a set (v) and a splice (v[]).
			"v.requests[].requestId", "v[].requestId",
			"v.requests[].responseId", "v[].responseId",
			"v.requests[].response[].id", "v[].response[].id", "v[].id",
			"v.requests[].response[].toolCallId", "v[].response[].toolCallId", "v[].toolCallId",
			"v.metadata.toolCallRounds[].toolCalls[].id",
			"v.metadata.toolCallRounds[].thinking.id",
			"v.metadata.toolCallRounds[].thinking.encrypted",
			"v.metadata.toolCallRounds[].statefulMarker",
			"v[].result.metadata.toolCallRounds[].toolCalls[].id",
			"v[].result.metadata.toolCallRounds[].thinking.id",
			"v[].result.metadata.toolCallRounds[].thinking.encrypted",
			"v[].result.metadata.toolCallRounds[].statefulMarker",
			"v.requests[].result.metadata.toolCallRounds[].toolCalls[].id",
			"v.requests[].result.metadata.toolCallRounds[].thinking.id",
			"v.requests[].result.metadata.toolCallRounds[].thinking.encrypted",
			"v.requests[].result.metadata.toolCallRounds[].statefulMarker",
			// Tool results keyed by tool call id: the key is the only join to the call.
			"v.metadata.toolCallResults.*",
			"v[].result.metadata.toolCallResults.*",
			"v.requests[].result.metadata.toolCallResults.*",
			// File URIs keyed by themselves: the workspace-storage hash must not tear the path.
			"v[].invocationMessage.uris.*", "v[].pastTenseMessage.uris.*",
			"v[].response[].invocationMessage.uris.*", "v[].response[].pastTenseMessage.uris.*",
			"v.requests[].response[].invocationMessage.uris.*", "v.requests[].response[].pastTenseMessage.uris.*",
			// The flat <id>.json store (chat.useLogSessionStorage off, transferred sessions) is the
			// same snapshot without the v wrapper.
			"requests[].requestId", "requests[].responseId",
			"requests[].response[].id", "requests[].response[].toolCallId",
			"requests[].result.metadata.toolCallRounds[].toolCalls[].id",
			"requests[].result.metadata.toolCallRounds[].thinking.id",
			"requests[].result.metadata.toolCallRounds[].thinking.encrypted",
			"requests[].result.metadata.toolCallRounds[].statefulMarker",
			"requests[].result.metadata.toolCallResults.*",
			"requests[].response[].invocationMessage.uris.*", "requests[].response[].pastTenseMessage.uris.*",
		},
		"pi": {
			"message.content[].id", "message.toolCallId",
		},
		"opencode": {
			"id", "parent_id", "session_id", "message_id", "data.parentID", "data.callID",
		},
		"hermes": {
			"tool_call_id", "tool_calls#json[].id", "tool_calls#json[].call_id", "tool_calls#json[].response_item_id",
		},
		"*": {"timestamp", "version"},
	}
}
