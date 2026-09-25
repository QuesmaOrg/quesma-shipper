# Pi, OpenCode and Hermes sessions

The source IDs are `pi-sessions`, `opencode-sessions` and `hermes-sessions`.
`PI_CODING_AGENT_SESSION_DIR` is a shipper override for a custom Pi `--session-dir`;
it must be set in the shipper process. Other supported root environment variables
are listed in the README. Arbitrary new root templates still require a catalog change.

These sources use normal source configuration, repository ignore markers, scrubbing,
encryption and upload state. Working directories remain in session records; no
project-map artifact is generated. No provider key is needed to collect
local history. These sources do not collect provider account or subscription quotas.

Pi ships its native JSONL format (session version 3 tested). OpenCode and Hermes use
the existing `sidecar` gather primitive with separate
`opencode_sessions` and `hermes_sessions` collectors. The SQLite file
itself never ships. Database locations are declared by YAML include patterns (`opencode.db`, or
`state.db` and `profiles/*/state.db`); bounded glob expansion avoids unrelated caches.
Agent-specific table/column queries run in one read-only transaction per
session, including committed WAL records. A failed, malformed or oversized read
produces no partial upload. The default snapshot cap is 64 MiB per session; discovery
refuses more than 100,000 sessions per database. Nested database/profile symlinks
are not followed. Agent credentials, model configuration and arbitrary database
columns are excluded.

Each derived JSONL line has `format_version: 1`, `type`, `session_id` and a stable
UUID-shaped `session_key`. Records with IDs also have `event_key`; OpenCode parts
carry `message_key` for joins. Raw IDs can be redacted by entropy scrubbing, so
consumers must use these stable keys. OpenCode `execution_key` identifies identical
copied fork history from content and creation time, excluding rewritten identity
references. Hermes `usage_id` distinguishes model/provider/billing/task scopes;
billing base URLs are hashed into that key but are not exported.

OpenCode emits `session`, `message`, and `part` records. Tokens/cost may appear in
both messages and step-finish parts: count assistant messages only. Hermes emits
`session`, `message`, and, when available, `usage` records. Usage is a cumulative
session/model/provider/task aggregate, not one event per API call. Its
`api_call_count` includes auxiliary tasks such as title generation. Session totals
overlap those rows and must not be added again. Old Hermes stores without the usage
table retain session totals with unknown call count and cost.

Database sessions are read again each sync because a WAL update need not change
the main database's stat. Deterministic output and the existing content fingerprint
suppress unchanged uploads; changed sessions replace the same logical object via
the normal generation mechanism. Large histories therefore have read cost on each
sync even when no upload is necessary. Custom `OPENCODE_DB` filenames and legacy
OpenCode JSON directory storage are not autodetected; this version targets the
current SQLite store.

## Validation

Tested locally on macOS with Pi 0.87.1, OpenCode 1.18.20 and Hermes 0.21.4
(Homebrew 2026.9.21). Tiny OpenRouter requests used Claude Haiku 4.5 and GPT-4.1
mini, including file reads, short resumed conversations, model changes, forks
(Pi/OpenCode), and harmless `printf` commands in a path containing spaces.
All three also answered a local MLX Qwen3.5-0.8B prompt without tool calls.
Direct Anthropic calls returned HTTP 401 with the available key; successful direct
Anthropic usage is not claimed. A first local Pi test mistakenly left read tools
enabled and hit its timeout; the corrected no-tools test passed.

Synthetic regressions cover live WAL updates, repeat snapshots, bounds,
cancellation, malformed data, older Hermes usage schemas, selected-column scope,
credential scrubbing, stable join keys, profile discovery, symlinks and repository
ignore markers. The opt-in `TestLiveAgentSessionStores` reads only the explicit
`SHIPPER_AGENT_TEST_STORES` directory and uses a fake upload sink. An optional
`SHIPPER_AGENT_TEST_EXPORT` writes scrubbed fixtures for downstream testing.
No personal history or API keys are committed as fixtures.

The full `make check` and Docker performance smoke tier passed. A stripped macOS
arm64 build grew from 16,068,066 to 16,118,722 bytes (+50,656; 0.32%), with no new
runtime dependency. On macOS the performance suite reports RSS; its Linux-only
VmHWM memory gate does not run.
