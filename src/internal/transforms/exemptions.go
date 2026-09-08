package transforms

// CompiledExemptions is the compiled baseline: an entry must be an exact field path naming
// a field that structurally cannot carry a user secret. Detector-scoped, so the pattern
// packs still scan these fields. The copy is fresh, since callers merge served additions in.
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
		},
		"codex": {
			// The rollout's tool-call join id (call_…): codex's tool_use_id.
			"payload.call_id",
		},
		"cursor": {
			"composerId", "bubbleId", "checkpointId", "requestId",
			// A hex-encoded image the backstop would destroy wholesale: a declared
			// opaque payload, keyed on the nested path.
			"content[].image.hex",
		},
		"project-map": {
			// Filesystem paths, project_dir arriving with __USER__ already baked in,
			// which ADDS entropy. Inert today (the inventory is raw-text scanned) so that
			// adding a jsonl sniff later cannot silently expose them to the backstop.
			"cwd", "project_dir",
		},
		"*": {"timestamp", "version"},
	}
}
