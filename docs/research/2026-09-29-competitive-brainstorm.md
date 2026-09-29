# Competitive research and improvement brainstorm

2026-09-29

Scope:
- **Notes.** Competitor research notes (written 2026-09-21 to 09-29) in `quesma.com/.claude/worktrees/breezy-bouncing-wigderson/tmp/precious-traces/`, cited as `N:`.
  - Landscape: `09-stash-like-projects.md`.
  - Demand evidence: `05-why-trajectories-matter.md`, `04-distillation-attacks.md`, `06-earendil-session-portability.md`, `07-swarmtraces.md`, `01-git-ai-openai.md`, `02-agent-trace.md`, `08`, and `outline/review-{ciso,cto,hn-reader}.md`.
- **Repos cloned 2026-09-29.**
  - `kenn-io/agentsview` @ `430b853`.
  - `datadog-labs/trajectory` @ `90c2786`: docs, installer and plugins only; the binary is closed source.
  - `entireio/cli` @ `fbd77e6`.
  - `specstoryai/getspecstory`, `tapes`, `memento`, `vibe-log`, `pond` (SHAs not recorded).
- **Shipper** checked at `main` `3c51cb3` (2026-09-29). Fleet Manager depends on `github.com/QuesmaOrg/shipper-protocol v0.1.0` (`fleet-manager/go.mod:12`).
- **Method.** Ideas came from five lenses: architecture, collection, security, consumption, fleet UX. An adversarial verifier checked each lens, and a critic pass rechecked the merged draft. This document applies both sets of corrections, merges duplicates and lists dropped ideas at the end.
- **Conventions.** Shipper paths are repo-relative, and competitor paths are relative to their clone. Numbers in worked examples are illustrative, not measurements.

## Landscape at a glance

| Competitor | Capture mechanism | Destination | Scrubbing | Client-side encryption | Normalized model | Notable features |
|---|---|---|---|---|---|---|
| **Quesma Shipper** | Polls native files every 15 min (`src/internal/config/schedule.go:9-13`); Cursor DB join enricher | Customer bucket via presigned PUT | 5 compiled packs on laptop, fail closed | age, to control-plane recipients | None: raw native files | Fleet Manager, TUF self-update, HMAC object keys |
| agentsview | fsnotify + per-agent Go parsers (73 registry entries, `internal/parser/types.go:152`); Hosted Raw Sync uploads originals | Local SQLite; PG/ClickHouse/DuckDB push; hosted server | Detect only (16 rules, `secret_findings`) | No ("not end-to-end encrypted", `docs/hosted-raw-sync.md:492`) | ~65-table SQLite | Append-suffix upload, FTS, cost, `s3://` source roots, A-F health score |
| Trajectory (Datadog) | 13 Claude hooks to localhost:19222; boundary hooks + rollout file for Codex; launchers | Datadog, OTLP, `local_forward_url` | Undefined `redact_pii` flag; detection markers | No | Canonical JSONL, unpublished, 30-day default retention | Markers, outcomes, cost per PR, MDM verify, convergence heartbeat, 22 supported + 14 preview agents (`README.md:170-209`; N:09:60) |
| Entire | Git hooks + agent hooks; shadow branches | Git refs in the developer's remote | 9 layers, but key-suffix skips (`*id`, `signature`, `path`) | No | Checkpoints | Commit trailers, line attribution, prefix redaction cache, OPF tier (network-backed, `redact/fingerprint.go:28-29`) |
| SpecStory | fsnotify on Claude dir; `run` wrapper | `.specstory/history` markdown; vendor cloud | Betterleaks only, fails open (`pkg/redact/redact.go:104-111`) | No (gzip over TLS) | Markdown + `SessionData` | FTS search, `/lore` skill mining; OTLP `prompt_text` unscrubbed |
| pond | Ingests 13 harnesses | Local dir or user's own S3 (Lance) | None at ingest | No (`docs/spec.md:106`) | Interlingua schema | Cross-client restore, `erase` + denylist, injected vs conversational parts |
| Tapes | JIT proxy, Envoy `ext_proc` | Postgres `raw_turns` | Not found | No | Derived sessions and spans | `rederive`, `raw equivalence` |
| Git AI | Pre/post edit hooks | `refs/notes/ai` + cloud or self-hosted store | Server-side on ingest | Store-side per N:outline/review-ciso.md:41 | Line attribution | `messages_url` pointer, transcripts kept out of git, access inherited from SCM (N:outline/review-ciso.md:41) |
| Stash | Agent hooks + raw JSONL backstop | Vendor cloud or self-hosted | None | No | Parsed server-side | Memory curator wiki; uploads touched files up to 1 MB (N:08:95) |
| vibe-log | Hooks + local read | Vendor cloud | Numbered placeholders on laptop | No | Reports | Privacy preview with redaction counts |
| Backplanes Spotlight | CLI after each session | Vendor cloud | Local + second server pass (N:09:118) | UNVERIFIED | UNVERIFIED | Org rollups |
| Anthropic Compliance API | Server-side, nothing installed | Anthropic, 6 years default | None ("Nothing masks...") | CMEK | API records | Excludes thinking, cloud sessions, Bedrock/Vertex; tool I/O truncated at 10,000 bytes |
| Claude Code / Codex native OTel | Built-in exporter | Any OTLP collector | Content off by default | No | OTel events | Claude `OTEL_LOG_RAW_API_BODIES=file:<dir>` |
| Langfuse / Braintrust | Stop hook / plugin hooks | Their backend | Not documented | No | Traces | Langfuse: hooks are "telemetry, not enforcement" |
| Copilot session streaming | Vendor side | SIEM; REST (UNVERIFIED) | Not documented | No | Events | UNVERIFIED: "EMU only", "REST last 48 h" and the `Agent-Logs-Url` commit trailer have no source in the notes |

Where Shipper is structurally different (checked against code):

- **Scrubs and seals on the laptop before upload.**
  - A scrub error means no upload (`src/internal/transforms/scrub.go:1-5`, `src/internal/engine/file.go:113-119`). Objects are sealed to served recipients (`src/app/app.go:165-183`). No surveyed competitor encrypts client-side.
  - Caveat: `allow_quesma_etl` defaults on (`README.md:149-159`), so "Quesma cannot read it" is false without a qualifier.
- **The control plane never touches bytes.**
  - Presigned PUTs go straight to the customer bucket.
  - The FM runtime has no read below `install=` except `tags.json`, pinned by `fleet-manager/src/terraform_test.go:237-280`.
  - agentsview's hosted server is both custodian and parser.
- **Raw native files, no client-side normalization.**
  - 3 families in 269 YAML lines (`src/internal/formats/catalogdata/`) vs agentsview's `internal/parser` package (408 `.go` files, 211 of them non-test). A file agent is cheap to add.
  - Nothing in the repo reads what Shipper writes: `transforms.Open` has no non-test caller (`src/internal/transforms/seal.go:192`).
- **Keyed object names** (`src/internal/formats/naming.go:95-103`) vs plaintext path layouts (agentsview S3 layout, pond). This is undercut by unkeyed plaintext `source-hash`/`shipped-hash` metadata (`src/internal/transforms/manifest.go:154-163`).
- **Zero footprint.**
  - No listener, no fsnotify, no agent config edits. "The shipper never writes inside an agent's store" is lint-enforced (`ARCHITECTURE.md:107-110`, `src/internal/platform/writepath_lint_test.go:78`).
  - The cost is up to 15 min latency and no session-boundary signal.
- **Whole-file re-ship on content change** (`src/internal/engine/file.go:85-111`), vs agentsview append-suffix objects (`internal/rawcapture/capturer.go:440-465`) and Entire's prefix cache (`cmd/entire/cli/checkpoint/redact_cache.go:27-47`).
- **Verified install.** The Linux installer pins a sha256 per asset and fails on mismatch (`src/packaging/linux/install.sh:79-88`). Trajectory's installer does not verify checksums (N:09:89).

## What competitors have that Shipper lacks

**Coverage**
- **More agents.**
  - Trajectory: 22 supported + 14 preview (`README.md:170-209`, N:09:60); `docs/CLIENT-INSTRUMENTATION.md:42` adds CodeWhale.
  - agentsview: 73 registry entries. pond: 13 harnesses.
  - Largest gaps: Copilot CLI, Gemini CLI, OpenCode, Pi, and the Claude-grammar siblings (Qwen Code, OpenClaude, iFlow, Qoder).
- **SQLite-backed agents** (Goose, Zed `threads.db`, Warp, ForgeCode `.forge.db`).
  - Trajectory passive DB sources (`docs/CLIENT-INSTRUMENTATION.md:25,43,44,46`).
  - agentsview `db_backed_provider.go` and online-backup capture (`internal/rawcapture/capturer.go:89,99`).
- **Low latency.** Wake hooks + authoritative file reconciliation (Trajectory `docs/CLIENT-INSTRUMENTATION.md:36-42`).
- **Injected context.** Effective system prompt and injected context via Claude Code raw API bodies on disk (code.claude.com/docs/en/monitoring-usage).

**Efficiency**
- **Append-suffix objects** (agentsview `capturer.go:440-465`), resumable tus-style upload (`internal/server/huma_routes_raw_upload.go:15`), and incremental parse from a byte offset (`internal/parser/provider.go:1040`).
- **Redaction prefix cache** keyed on a rule-config fingerprint (Entire `redact/fingerprint.go:12-40`).

**Scrub quality**
- **DSN / connection-string layer**, placeholder allowlist, and pack `samples[]` self-tests (Entire `redact/redact.go:43-46`, `:77-90`, `packs.go:67`).
- **Splice verification** (`ensureReplacementsLanded`, `redact.go:1245-1300`).
- **Signature exemption.** Entire skips any JSON key ending in "signature" for Claude Code (`redact/redact.go:1338-1344`).
- **ML PII tier:** Entire OPF, pre-push only (`docs/security-and-privacy.md:247`). Not copyable: OPF is network-backed (`redact/fingerprint.go:28-29`), which N6 rules out.
- **Rules version** stored per session (agentsview `docs/session-api.md:1091-1093`).

**Privacy and governance**
- **Capture opt-outs at three scopes (Trajectory).**
  - User-wide: `trajectory config capture disable`, persisted in `~/.trajectory/capture.disabled`; running servers discard new events immediately (`docs/PRIVACY.md:73-84`).
  - Per session: `/incognito`, which keeps local JSONL capture (`plugin/trajectory/skills/incognito/scripts/toggle.sh:445-447`).
  - Per process: `TRAJECTORY_DISABLED=1` (N:09:75).
- **In-content exclusion markers:** claude-mem `<private>` tags (N:09:164) and Trajectory's `<sensitive>` convention (N:09:77).
- **Per-session erasure** with a resurrection denylist (pond `docs/spec.md:506`).
- **Repo-level config**, applied only for trusted origins and only allowed to narrow (Trajectory `docs/REPO-MARKERS.md:39-44,118-120`).
- **Execution-granting settings** honoured only from an untracked local file (Entire `docs/security-and-privacy.md:198-215`).
- **Retention knob** (Trajectory `capture.retention_days`, `docs/CONFIGURATION.md:420`) and content-class levels (agentsview `archive_content`, `docs/configuration.md:60-104`).
- **Local key custody in the OS keychain** (Trajectory, N:09:78). Shipper keeps its age identity and `name_key` in a plain 0600 `identity.json` (`src/internal/identity/identity.go:27-28,48-55`).
- **Access inherited from the SCM** (Git AI, N:outline/review-ciso.md:41).

**Fleet operations**
- **Convergence heartbeat and capture-gap metrics** (Trajectory `docs/METRICS-REFERENCE.md:1337-1360,1457-1462`).
- **MDM tooling:** prepare/verify, plus managed status with `reason`/`next_step` (`docs/MANAGED-ENDPOINTS.md:75-86`).
- **Managed tags and cohort overlays** (`docs/CONFIGURATION.md:142-165`, `docs/USER-GUIDE.md:1375`).
- **Release control:** pinned versions, rollback, `update converge` (`docs/USER-GUIDE.md:246-253`).
- **Support bundle** `flare` (`docs/USER-GUIDE.md:145`).
- **Operator health endpoint** with failure classes (agentsview `docs/hosted-raw-sync.md:409-418`).

**Consumption**
- **A reader at all:** search, cost dashboards, resume (agentsview, SpecStory, pond). agentsview reads `s3://` roots directly (`docs/configuration.md:1256-1332`).
- **Pure re-derive** over a verbatim store (Tapes `cmd/tapes/dev/rederive.go`, `cmd/tapes/raw/equivalence.go`).
- **Honest cost semantics.**
  - Trajectory `cost_status` priced/unpriced/unavailable/invalid (`docs/LLM-OBS-SPAN-TAGS.md:42`).
  - agentsview ccusage-style dedupe (`internal/parser/types.go:1534-1540`).
- **Commit linkage:** trailers (Entire `cmd/entire/cli/trailers/trailers.go:17-59`), git notes (Git AI, memento), and a CI coverage gate (memento).
- **Cross-client restore** (pond `docs/spec.md:35`, Trajectory `resume --target`).
- **Taxonomies:** tool taxonomy (Trajectory `docs/SECURITY-EVENT-STREAM.md:86-96`) and injected vs conversational parts (pond `docs/spec.md:429`).
- **Coverage cross-check** against a vendor-side record (Anthropic Compliance API, Claude Enterprise only).

## Architecture ideas

### A1. Record the effective scrub configuration in every manifest
- **Problem:**
  - After a detector fix, nobody can list the objects scrubbed under the old rules.
  - `client.version`/`commit` (`src/internal/transforms/manifest.go:88-95`) pin the compiled corpora. Served and local `rule_packs`, `secret_key_names` (`src/internal/config/document.go:85-90`) and `structural_exempt` (`document.go:31`) are recorded nowhere.
  - `config_version` is the schema version, pinned to `[1]` (`src/internal/config/compiled.go:9-10`).
  - An already-shipped file never re-scrubs, because its content hash is unchanged (`src/internal/engine/file.go:85-105`).
- **Proposal:**
  - Add `redaction.scrub_fingerprint` inside the sealed manifest, next to the existing `rule_hits` and `scan_mode` (`manifest.go:42,73-77`). It is a sha256 over pack names, per-pack corpus digest, entropy thresholds (`heuristic.go:30-35`), exemption baseline, effective served knobs and an engine version constant.
  - Put it in the manifest only, with no plaintext header.
  - Step 2: reuse the existing re-ship lever instead of a new one. When a source's `SpecFingerprint` changes, `EnsureSpec` drops all of that source's local entries and the whole source re-ships (`src/internal/engine/state.go:249-266`, `src/internal/sources/spec.go:174`). Mix `scrub_fingerprint` into that path when a release is flagged as a leak fix. Keep a per-run budget, because a bump re-ships the whole source in one go.
  - Example: `"redaction":{"density":0.0021,"rule_hits":{"github-pat":1},"scan_mode":"json","scrub_fingerprint":"sp1:9f2c..."}` (`scan_mode` value illustrative).
  - The reader (A3) can then list objects whose fingerprint predates a fix. Re-scrub on read is safe because the matcher skips existing sentinels (`matcher.go:25-28`).
- **Evidence:**
  - Entire: `ConfigFingerprint()` with a bump-on-change constant (`redact/fingerprint.go:12-40`).
  - Backplanes: second server pass (N:09:118).
  - agentsview: `secrets_rules_version` per session (`docs/session-api.md:1091-1093`).
- **Constitution fit:** Art. 4 (the scrub stage is configurable) becomes auditable per object. The re-ship step touches Art. 7. It is a manifest schema and golden change, so it needs confirmation (AGENTS.md style rule 5).
- **Value:** 4/5 · **Effort:** S (manifest field), M (re-ship with budget)
- **Risks:**
  - A forgotten constant bump makes the fingerprint lie. Hash the corpora in a test.
  - Mixing a global fingerprint into `SpecFingerprint` re-ships every source on a bump; mix it in only for flagged releases.
  - Every re-ship adds a full noncurrent version.
  - Older versions stay under the weaker rules until retention removes them.

### A2. Drop the Fleet Manager dedup probe
- **Problem:**
  - FM HEADs data objects to answer `already_present` (`fleet-manager/src/upload.go:218-275`, `fleet-manager/src/aws.go:100-107`).
  - The AWS template grants the runtime only `PutObject`/`PutObjectTagging` on the data prefix, plus `GetObject` on `*/tags.json` (`fleet-manager/terraform/aws/main.tf:167-181`). S3 HEAD requires `s3:GetObject`, so on template deployments the probe most likely always errors.
  - GCP fails the same way: the test forbids `storage.objects.get` on the data role (`fleet-manager/src/terraform_test.go:274-276`).
  - Today's impact is small. Failed reads are logged and the objects are authorized as new (`upload.go:287-289`). The cost is a wasted HEAD per object plus a log line per batch, not data loss.
- **Reproduction:** deploy the AWS template, enroll one install, ship one file, change nothing and force a second authorize for the same key, then grep FM logs for `deduplication probe for install ... reads failed` (`upload.go:287-289`).
- **Proposal:**
  - (a) Drop the probe. Do not replace it with FM's record of authorized keys: FM records what it authorized, which does not prove the PUT landed ("Last vend means tickets were issued, not that the upload finished", `fleet-manager/README.md:190-191`). The probe code states the stakes: "a missed match costs one upload, a wrong match loses the object" (`upload.go:222-224`).
  - (b) Grant read, which `main.tf:175-181` explicitly rejects as "read access to every sealed payload in the bucket".
  - Recommend (a).
  - F9's re-ship ratio may use FM's key record as a metric, but that record must never feed `already_present`.
- **Evidence:** Shipper only; raised independently by the architecture and fleet verifiers.
- **Constitution fit:** Art. 2 and 5 unchanged under (a). Template grant changes need confirmation (`fleet-manager/AGENTS.md` rule 2).
- **Value:** 2/5 · **Effort:** S
- **Risks:** Runtime behaviour is UNVERIFIED until the reproduction runs. On a non-template deployment that does grant read, dropping the probe costs re-uploads that the probe saves today.

### A3. `quesma-reader`: a customer-run module to list, open and export the bucket
- **Problem:**
  - Nothing reads what Shipper writes. `transforms.Open` and `ReadManifestPrefix` (`src/internal/transforms/seal.go:192,227`) have no non-test callers.
  - The documented path is `age -d | zstd -d | tar` (`README.md:256-262`).
  - Leaf names are HMACs of paths (`naming.go:95-103`), so finding a session means decrypting manifests.
- **Proposal:**
  - A separate Go module with its own release line, like `fleet-manager/`, never linked into the shipper.
  - Ranged manifest fetch (`SuggestedPrefixBytes`, `seal.go:32-35`).
  - A local catalog: key, version id, install name from `tags.json`, source_id, native_path, sealed_at, rule_hits.
  - Hard error on an unknown `manifest_version` (already in `DecodeManifest`, `manifest.go:136-139`).
  - Example:
    - `quesma-reader index s3://acme-traces/v1/organization=acme/ -i etl.agekey --out acme.catalog.duckdb`
    - `quesma-reader cat --install 'Rafal laptop' --path '/Users/__USER__/.claude/projects/-src-api/4f1c.jsonl'`
    - `quesma-reader export --since 30d --to ./archive/`
  - Hosts F3, F16, F17, F22, F23 and A8.
  - Optional later: authorize reads by SCM repo membership (Git AI precedent, N:outline/review-ciso.md:41), keyed on A7's sealed `repo.remote_url`. Example: a developer with read on `github.com/acme/api` can `cat` sessions attributed to it and nothing else. This needs the reader to run as a service holding the key, so it is a separate decision.
- **Evidence:**
  - agentsview reads `s3://` roots with ETag/version skip (`docs/configuration.md:1256-1332`).
  - pond's rule that the archive survives loss of the source (`docs/spec.md:498`); Tapes `rederive`.
  - CTO: "if I stop using Shipper, do I still have plain JSONL I can read" (N:outline/review-cto.md:52).
- **Constitution fit:**
  - Art. 2: runs in the customer environment, and FM keeps no decrypt path (`README.md:641`).
  - Art. 4: encrypt-only installs stay encrypt-only.
  - `internal/` packages of `github.com/QuesmaOrg/quesma-shipper` (`src/go.mod:1`) are importable only under that path prefix. So either live under it or promote a small public seal/manifest package.
- **Value:** 5/5 · **Effort:** M
- **Risks:**
  - It becomes a second consumer of the manifest contract.
  - `--all-versions` needs `s3:GetObjectVersion`, which `fleet-manager/src/terraform_test.go:15` forbids in the shipped template, so it needs a customer-held role.
  - Custodian keys on analyst machines widen exposure. Steer users to an ETL-only recipient.

### A4. Key the plaintext content hashes, and write the threat model
- **Problem:**
  - Every object carries `source-hash` (an unkeyed SHA-256 of the pre-redaction bytes) and `shipped-hash` as plaintext metadata (`src/internal/transforms/manifest.go:31-34,154-163`).
  - FM also receives `source_hash` in every authorize request (`fleet-manager/src/upload.go:53,111,190`).
  - So a bucket reader or FM operator (Quesma SaaS included) can confirm a guessed file. Example: a `settings.json` from a known template with a weak password.
  - This contradicts "no contents" (`README.md:29-30`). Object names are keyed for exactly this reason (`naming.go:95-97`).
- **Proposal:**
  - Send `HMAC-SHA256(name_key, "source:" || raw)`, and likewise for the shipped hash, in metadata and authorize. Keep the raw SHA-256 only inside the sealed manifest.
  - Dedup still works: comparison is per key, and `name_key` is client-minted (`src/internal/identity/identity.go:91-99`).
  - Add `THREAT_MODEL.md` with actors (bucket admin, ETL reader, Quesma ETL key holder, compromised laptop, compromised FM, malicious served config) and what each can read, link and alter.
- **Evidence:**
  - No competitor encrypts (agentsview `docs/hosted-raw-sync.md:492`, pond `docs/spec.md:106`), so Shipper's lead rests on metadata hygiene.
  - Transcripts are traded (N:04:148).
- **Constitution fit:** Art. 4. shipper-protocol change.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Mixed fleets lose dedup until FM accepts both forms.
  - Stored objects keep the oracle.
  - The realistic threat is low-entropy guessing; do not overstate it.
  - `name_key` itself sits in a plain file (A19), so keying the hashes moves the secret, it does not remove it.

### A5. Move per-agent Go knowledge into the catalog
- **Problem:** A new family silently loses repo attribution, `.notrajectories` and join-key exemptions.
  - The cwd probe is compiled to two source ids (`src/internal/sources/ignore.go:58-64`).
  - Structural exemptions are keyed by family (`src/internal/transforms/exemptions.go:9`), so the entropy backstop would shred a new family's uuid/parentUuid keys.
  - Cursor already ignores `.notrajectories`.
  - The retention trap is only a YAML comment (`catalogdata/claude-code.yaml:9-11`).
- **Proposal:**
  - Optional catalog fields:
    - `attribution: {cwd_fields: [cwd, payload.cwd], scan_bytes: 65536}`, or a path-slug rule;
    - `retention_hint: {setting: cleanupPeriodDays, default_days: 30}`;
    - `format_provenance: {source: public|docs|none, ref: <url>}`;
    - exemption baselines in YAML.
  - Per-family deny entries (most already exist in `sources/deny.go:46-52`) move next to the family. They stay add-only and never servable.
  - Every family file ships a sanitized fixture under `catalogdata/testdata/<family>/`, which the conformance test runs through sniff, scrub and seal.
  - Example: `opencode.yaml` declares its cwd field and immediately participates in `tracking` and `.notrajectories`.
- **Evidence:** agentsview `AgentDef` (`internal/parser/types.go:96-142`), `RemoteSyncExcluded` (`types.go:388,511,760,1097`), format-provenance inventory (`docs/internal/session-format-sources.md`).
- **Constitution fit:**
  - Art. 3.
  - Schema extension, since `source-spec.schema.json` has `additionalProperties: false`.
  - `SpecFingerprint` must exclude attribution fields, or the fleet re-ships (`src/internal/sources/spec.go:173-195`).
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Declarative paths cannot express DB joins.
  - It is UNVERIFIED that Cursor transcripts carry a cwd (`ignore.go:17-21`), so Cursor may need its lossy workspace slug (`cursor.yaml:26-43`).

### A6. Let Fleet Manager serve data-only sources over compiled primitives
- **Problem:**
  - Adding a file agent needs a release, a TUF publication and a fleet rollout.
  - Config cannot create a source (`src/internal/config/resolve.go:180-185`). Compiled roots are still enforced: a served root outside the compiled set is rejected as "outside the compiled scope ceiling ... a new root requires a release" (`resolve.go:501-505`). Served layers can widen only `include`/`exclude` within compiled roots. shipper-protocol v0.1.0 pins both rules (PROTOCOL.md:363,366).
  - Art. 1 repealed "v1's compiled-in scope ceiling" (`CONSTITUTION.md:19-20`), yet the code still enforces and names it. A6 closes that gap between the constitution and the code.
- **Proposal:**
  - Served `sources_added` with these guards:
    - `gather: file_glob` only;
    - `scrub` forced true and unsettable;
    - `sniff` mandatory;
    - compiled deny list and symlink checks applied;
    - roots restricted to a compiled allowlist of agent-shaped dirs under `$HOME`;
    - a local `enabled: false` wins.
  - `config --with-provenance` shows "served by org acme".
  - Depends on A5.
  - JSON-document sources get only raw-text scanning today (`src/internal/engine/file.go:113`), so extend the JSON-aware scrub to `sniff: json`.
  - Example: `{id: gemini-cli-chats, family: gemini-cli, gather: file_glob, roots: [~/.gemini], include: ['tmp/*/chats/session-*.json'], sniff: {kind: json}}` (paths UNVERIFIED).
- **Evidence:**
  - agentsview needs Go code and a release per agent (`internal/parser/types.go:152`).
  - Trajectory's newest integrations are mostly file shapes (`docs/CLIENT-INSTRUMENTATION.md:36-42`).
- **Constitution fit:** Art. 1 and 3; Art. 4 via forced scrub. Needs a protocol rulebook release.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - A compromised FM widens collection. Part of this exists today:
    - served layers can replace `include`/`exclude` on existing sources, with `roots` held inside the compiled set (`resolve.go:343-354,501-505`);
    - glob narrowness is unchecked (`config/compiled.go:5-6`);
    - so a directory under `~/.claude` is reachable today by widening `claude-code-context`.
  - The allowlist bounds the new exposure.

### A7. Ship repo attribution inside the sealed manifest
- **Problem:**
  - Cost by repo, PR and incident joins, and per-repo consent all need repo identity.
  - Shipper resolves each session's checkout locally, with worktree folding (`ignore.go:15-64`, `:36-40`), and throws it away. The manifest has no repo field (`manifest.go:19-69`).
  - Codex already records the git remote natively (`codex.yaml:1-3`), so the gain is mainly for Claude Code (and Cursor after A5).
  - A downstream already derives repo from the `cwd` basename (`exemptions.go:26-28`), which loses owner and remote.
- **Proposal:**
  - Add a sealed `repo {remote_url, main_checkout_path, branch_at_seal, head_at_seal}`.
  - Extend the existing bounded `.git` reader, `GitRead{WalkUp, FollowGitdirFile}` (`src/internal/sources/ignore.go:50-54,62`), to read `HEAD`, refs, `packed-refs` and `config`: no git exec, no network. The remote URL passes the scrubber (userinfo tokens).
  - Implement it in the engine beside `RepoFilter`, not as an enricher: enrichers have "no filesystem access" (`src/internal/transforms/enrich.go:33-35`).
  - Successive versions give a HEAD sequence, so commits made during a session fall out of `git log first..last` downstream.
  - Example: `"repo":{"remote_url":"github.com/acme/api","branch_at_seal":"fix/login-timeout","head_at_seal":"e4f5a6b"}`.
- **Evidence:**
  - Git AI `refs/notes/ai` with `messages_url` (N:01); Entire trailers (`trailers.go:17-59`); memento CI `gate`.
  - Trajectory attributes PRs from bounded Git state with no provider API (`docs/USER-GUIDE.md:1481-1507`).
  - Claude `OTEL_METRICS_INCLUDE_REPOSITORY` defaults to `false` and derives the repo from the `origin` remote (code.claude.com/docs/en/monitoring-usage, fetched 2026-09-29).
- **Constitution fit:** Art. 4: inside the envelope, never plaintext, like `native_path` (`manifest.go:154-157`). Needs a shipper-protocol and golden change.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - HEAD at seal is not HEAD per turn; commits reset between flushes are missed.
  - Cursor is not covered until A5.

### A8. `derive`: normalized tables as a pure re-derive over the bucket
- **Problem:**
  - Agent files are undocumented and drift (N:05:457,461).
  - Every reader must rebuild Claude fork and subagent linking, Codex live/archived collapse, Cursor joins and usage dedupe.
- **Proposal:**
  - A reader stage writes versioned Parquet (`sessions`, `messages`, `tool_calls`, `usage`) to a customer destination that is not the shipper bucket.
  - What it keeps:
    - pond's `conversational`/`injected` part provenance;
    - Trajectory's `tool_operation`/`tool_name`/`native_tool_name` triad;
    - verbatim per-event timestamps;
    - `derivation_version`;
    - row provenance `(object_key, version_id, line_no)`.
  - Output two physically separate datasets:
    - `content/` for custodians, optionally partitioned by repo so access can follow SCM membership (A3);
    - `metrics/` (counts, tokens, durations; no text, no paths) with a minimum group size for manager dashboards.
  - Example: `quesma-reader derive --to s3://acme-analytics/trajectories/v3/ --metrics-min-group 5`. A parser fix is `derive --rebuild`, never re-collection.
- **Evidence:**
  - Tapes keeps turns verbatim "so fields unknown to this build survive for later derivers" (`migrations/1781049035_raw_turns.up.sql:19`).
  - pond `docs/spec.md:429,478`; Trajectory `docs/SECURITY-EVENT-STREAM.md:86-96`.
  - agentsview `archive_content: usage` (`docs/configuration.md:60-104`).
  - CISO aggregate-only ask (N:outline/review-ciso.md:22,39); Git AI inherits access from the SCM (N:outline/review-ciso.md:41).
- **Constitution fit:** Art. 2. Derived plaintext is outside Shipper's encryption guarantee; document that.
- **Value:** 5/5 · **Effort:** L
- **Risks:**
  - Parser upkeep is agentsview's cost; keep to collected families.
  - It overlaps the Quesma ETL, which is not in this repo.
  - Public vs internal schema is Q9.

### A9. Customer-signed recipient policy
- **Problem:**
  - Served config is authenticated only by TLS. `configResponse` is `{config, expires_at}` (`fleet-manager/src/server.go:91-94`), and the client verifies nothing (`src/internal/controlplane/backend.go:179-194`).
  - Whoever operates or compromises FM can add an age recipient.
  - Quesma's recipient is already added by default (`fleet-manager/src/model.go:43-47,82-84`; `server.go:269-286`).
  - One admin credential serves all orgs (`model.go:181`, `admin_http.go:144-160`).
- **Proposal:**
  - Pin an org policy Ed25519 key out of band: MDM profile, or pasted at login. Not via the grant, which FM mints.
  - Recipients, `allow_quesma_etl`, `include_install_recipient` and served `structural_exempt` arrive in a detached, customer-signed document.
  - On a bad signature the client keeps the last good set and reports `policy_signature_invalid`. Operational fields stay FM-editable.
  - Example: `shipper-policy sign --key awskms://acme/shipper-policy recipients.yaml`. An injected `age1evil...` is refused on every laptop.
  - Pair with per-org admin identity.
- **Evidence:**
  - Trajectory managed-authoritative fields (`docs/CONFIGURATION.md:28-44`).
  - Entire honours execution settings only from an untracked local file (`docs/security-and-privacy.md:198-215`).
- **Constitution fit:**
  - Art. 4 ("the control plane supplies the recipients", `CONSTITUTION.md:39-41`) still holds, as "delivers customer-signed recipients"; add an amendment note.
  - Protocol change.
- **Value:** 5/5 · **Effort:** L
- **Risks:**
  - A lost policy key blocks rotation, so a recovery path is needed.
  - The MDM rollout must precede enforcement.

### A10. Labels at enrollment and config overlays by label
- **Problem:**
  - There is one `AuthoredYAML` per org (`fleet-manager/src/model.go:30-40`), rendered for every install (`server.go:269-286`).
  - `TagsRecord` holds only `Name` (`model.go:150-155`), and grants carry no metadata (`model.go:157-164`).
  - So there is no per-team scope, no canary ring, and no team slice downstream.
- **Proposal:**
  - Grants and invites carry labels (`team=payments, ring=canary`), which are copied into `tags.json` so bucket walkers can resolve team without FM.
  - FM merges the org YAML and matching overlays server-side and serves one document, so the client rulebook is unchanged (one served tier).
  - Specify FM-side precedence, for example whether an overlay may re-enable a source the org YAML disabled. Show the rendered config per install.
  - Example: `overlays: [{match: {team: contractors}, yaml: "sources: {cursor-transcripts: {enabled: false}}"}]`.
- **Evidence:**
  - Trajectory managed `tags` and cohort layering (`docs/CONFIGURATION.md:142-165`, `docs/USER-GUIDE.md:1375`).
  - CloudWatch Coding Agent Insights reportedly slices by department, team and cost center (UNVERIFIED: no source in the notes).
- **Constitution fit:** Art. 1 and 3. No client or protocol change if FM renders.
- **Value:** 5/5 · **Effort:** L
- **Risks:**
  - A team of one identifies a person.
  - Precedence bugs can widen scope.
  - An extra `tags.json` read per config request needs caching.

### A11. Sealed session reference for subagents, spills and erasure
- **Problem:**
  - One session spans the transcript, subagent transcripts with `.meta.json`, `tool-results/` spills, Cursor nested files and derived objects.
  - Keys are opaque path HMACs, so grouping requires decrypting everything, and per-session erasure has no handle.
- **Proposal:**
  - Sealed `session_ref = HMAC(name_key, "session:"+family+":"+native id)` and `parent_session_ref`.
  - Derive them from path grammar, as in Codex identity collapse (`codex.yaml:50-55`) and the `.meta.json` join (`claude-code.yaml:18-22`), not from content.
  - Plaintext `session-ref` metadata is a separate later decision: it needs vend-ticket signing on client and FM, and it leaks objects-per-session.
  - Example: one Claude session under `~/.claude/projects/-src-api/` ships six objects: `4f1c.jsonl`; two subagent transcripts `4f1c/subagents/agent-a.jsonl` and `agent-b.jsonl`, each with its `.meta.json` sibling (collected as separate objects, `claude-code.yaml:18-23`); and one `4f1c/tool-results/` spill (file names illustrative). All six carry `session_ref=s:7d31...`; the two subagent transcripts also carry their own `session_ref` and `parent_session_ref=s:7d31...`. Erasing `s:7d31...` then finds all six without decrypting unrelated objects' payloads.
- **Evidence:** pond `erase` (`docs/spec.md:506`); agentsview parent session + relationship type (`internal/parser/types.go:1349`); Trajectory `docs/SUBAGENT-TRACE-MODEL.md`.
- **Constitution fit:** Art. 4 (sealed); Art. 5 unaffected. Needs a protocol and golden change.
- **Value:** 3/5 · **Effort:** M sealed only; L with plaintext metadata
- **Risks:**
  - Wrong grouping makes erasure incomplete.
  - Identity must be stable across agent versions.
  - Erasure must also delete noncurrent versions.

### A12. Per-class recipient sets
- **Problem:**
  - One recipient list serves every object (`src/app/app.go:167-183`).
  - The org key reads identity as well as content: account objects ship unscrubbed, with decoded Codex id_token claims (`src/internal/sources/accounts_codex.go:32-37`, `README.md:422-424`).
- **Proposal:**
  - Served `encryption.classes: {account: [age1hr...], trajectory: [age1sec..., age1etl...], heartbeat: [age1ops...]}`, each union-only and never empty.
  - The manifest already carries `artifact_class` (`manifest.go:29`).
  - Sell it as access separation, not pseudonymity.
  - FM already serves `include_install_recipient: false` by default (`fleet-manager/src/admin-ui/app.js:125`, `server.go:278-280`).
- **Evidence:** Trajectory privacy-reduced spans drop identity (`docs/LLM-OBS-SPAN-TAGS.md`). Claude Code OTel includes `user.email` "always included when available", with no opt-out (code.claude.com/docs/en/monitoring-usage, fetched 2026-09-29).
- **Constitution fit:** Art. 4 ("possibly several" recipients). Protocol change plus a rulebook row.
- **Value:** 4/5 · **Effort:** L
- **Risks:** Transcripts re-identify people. More keys to lose; pairs with custody attestation (F14).

### A13. Envelope epoch keys for rotation, crypto-shredding and decrypt audit
- **Problem:**
  - Objects are sealed straight to custodians.
  - Recipient changes never affect old objects, and total key loss is silent (`README.md:141-147`, `fleet-manager/OPERATIONS.md:31-33`).
  - Org deletion waits on "key destruction" answers (`OPERATIONS.md:45-47`).
  - Nothing logs decryption.
- **Proposal:**
  - Seal to a per-(org, install, month) epoch X25519 recipient.
  - The epoch identity is sealed to custodians, optionally KMS-wrapped with encryption context {org, install, epoch}, at `v1/organization=acme/keys/install=<id>/epoch=2026-10.age`, outside object-lock scope.
  - Effects:
    - rotation re-wraps small objects;
    - deleting every version of a key object crypto-shreds an install-month;
    - KMS Decrypt logs give a decrypt audit.
  - Minting: never on the client. Art. 4 says "The client needs no ability to decrypt what it ships" (`CONSTITUTION.md:41-42`), and a client that mints its own epoch keypair can decrypt that epoch. Mint via KMS data keys or custodian batches, and serve only the epoch public key.
  - Shredding runs in a separate operator tool under its own principal (working name `shipper-ops`), not in FM: "A change that needs a payload, a database, a lock, or a delete is not a fleet-manager change. It is a constitutional one." (`fleet-manager/ARCHITECTURE.md:80-81`). Example: `shipper-ops keys shred --install 3f9e... --all-epochs`.
- **Evidence:**
  - No competitor crypto-shreds.
  - Anthropic hard delete reportedly has "no recovery window" (UNVERIFIED: cited as N:05:661, but no note contains the quote); CMEK exists there (N:09:19).
- **Constitution fit:**
  - Art. 4 holds only with server-side or custodian minting.
  - Art. 2 is strained if FM ever holds a plaintext epoch key.
  - File a `CONSTITUTION.md` candidate first (the section is currently empty, `CONSTITUTION.md:65-68`), covering both the key store and the delete path.
- **Value:** 5/5 · **Effort:** XL
- **Risks:**
  - The `keys/` prefix becomes critical to back up.
  - `allow_quesma_etl` becomes per-epoch unwrap grants.
  - Cached unwrapped keys defeat shredding.

### A14. Append-only segment upload for growing JSONL
- **Problem:**
  - A changed file re-ships whole onto one key (`src/internal/engine/file.go:107`, `naming.go:95-97`).
  - A hot session adds a full-copy version on each content-changing tick, so bytes grow roughly quadratically with session length.
  - There is no idle gate. No volume number exists (N:outline/review-cto.md:23,54).
- **Proposal:**
  - Interim (S): a quiescence gate that delays re-shipping a growing file until it is idle for N minutes, with a hard max delay. Measure the re-ship ratio with F9.
  - Full design (XL), for `sniff: jsonl` sources only:
    - the fingerprint gains committed offset, resumable prefix SHA-256, a 64 KiB tail anchor, and inode/device;
    - if the file only grew, scrub only the new complete lines (JSONL scrub is per-line stateless, `scrub.go:249,261-312`);
    - seal the new lines as a content-addressed segment written If-None-Match;
    - rewrite a small head object at the existing key.
  - Shrink, inode change, anchor mismatch or an A1 fingerprint change triggers a full rebase, which also compacts.
  - Example: head `.../mirror/source=claude-code-transcripts/<hmac>.age`, segments `.../<hmac>/seg/000042-<sha256>.age`.
- **Evidence:**
  - agentsview suffix objects (`internal/rawcapture/capturer.go:440-465`) and offset parse (`internal/parser/provider.go:1040`).
  - Entire's prefix cache came after a 70 MB Codex rollout cost ~67 s per Stop hook (`cmd/entire/cli/checkpoint/redact_cache.go:27-47`).
- **Constitution fit:**
  - Art. 5 strengthened (content-addressed segments).
  - Art. 7: offsets commit after confirmation.
  - Protocol change: key grammar and FM regex (`fleet-manager/src/upload.go:95`).
- **Value:** 4/5 · **Effort:** XL (interim gate S)
- **Risks:**
  - Noncurrent versions are the designed history mechanism (`naming.go:96-97`), and segments change that.
  - Shipped bytes are zstd-compressed, so real savings are smaller than raw arithmetic (UNVERIFIED).
  - Exclude enriched sources: cursor-transcripts needs the whole file (`cursor.yaml:44-45`).
  - Needs A3 to reassemble.
  - Rebase rate on real fleets is UNVERIFIED.
  - The interim gate delays active sessions.

### A15. `db_export` primitive for SQLite-only agents
- **Problem:**
  - Goose, Zed `threads.db`, Warp, ForgeCode `.forge.db` and older Cursor conversations exist only in SQLite (`cursor.yaml:32-34`). Codex DB stores are deferred (`codex.yaml:5-7`).
  - SQLite bytes are refused as opaque (`scrub.go:345-346`).
  - Only the `file_glob` and `account` primitives exist (`src/internal/sources/gather.go:276-283`).
- **Proposal:**
  - Promote the `sqliteread` ladder (read-only open, then `VACUUM INTO`, then cold copy; `sqliteread.go:85-126`) into a compiled primitive.
  - Per family, compiled: DB candidates, table, grouping key, columns, row cap, and a row deny list like `sqliteread/deny.go:12-30`.
  - Output one JSONL object per session through the normal scrubber, with `db_provenance`.
  - Truncation is already reported (`sqliteread.go:48-49,68`); surface it in the heartbeat.
  - Example: `{id: goose-sessions, gather: db_export, db: {candidates: [~/.local/share/goose/sessions/sessions.db], table: messages, group_by: session_id}}` (schema UNVERIFIED).
- **Evidence:** agentsview `db_backed_provider.go` and online backup (`capturer.go:89,99`); Trajectory passive DB sources (`docs/CLIENT-INSTRUMENTATION.md:25,43,44,46`).
- **Constitution fit:**
  - Art. 3.
  - Rulebook decision: today a served layer may disable but never enable an enricher, because it reads a DB the raw pipeline never touches (`resolve.go:365-367`; PROTOCOL.md:365). A centrally enabled `db_export` reverses that.
- **Value:** 3/5 · **Effort:** L
- **Risks:**
  - Vendor schema drift.
  - Needs per-session watermarks.
  - It ships a Shipper-defined projection, not vendor bytes.
  - OpenCode also keeps file storage (agentsview `types.go:284-296`), so it may not need this.

### A16. Split the FM control store from per-org data sinks (only if SaaS)
- **Problem:**
  - Each deployment has one `--bucket` (`fleet-manager/src/main.go:54`), holding both `v1/control/...` and install data (`fleet-manager/OPERATIONS.md:98-108`).
  - A multi-tenant Quesma FM would therefore hold every customer's objects.
- **Proposal:**
  - Org-level `sink` records: AWS role + external id, GCS impersonation, Azure user-delegation SAS. The signer is resolved per request.
  - The customer template grants only PutObject + tagging under `v1/organization=acme/`.
  - Per-org sinks repeat the startup checks, e.g. versioning (`terraform/aws/main.tf:182-186`).
- **Evidence:**
  - agentsview isolates tenants with RLS but holds the data (`docs/hosted-raw-sync.md:41,492-499`).
  - pond writes to the user's bucket but has no fleet (`README.md:16,76`).
- **Constitution fit:** Art. 1, 2, 6.
- **Value:** 3/5 · **Effort:** L
- **Risks:** Quesma would hold standing write authority into every customer bucket. Whether SaaS is planned is UNVERIFIED (Q13).

### A17. Two-pass streaming seal to lift `max_file_bytes`
- **Problem:**
  - Codex rollouts above 512 MiB and Claude transcripts above 256 MiB never ship (`codex.yaml:39-43`, `claude-code.yaml:39`).
  - The payload is held several times in memory (`src/internal/platform/memstat.go:66-73`).
- **Proposal:**
  - Single-pass streaming does not fit the format:
    - the manifest comes first and carries `ShippedHash`/`PayloadSize` over the whole payload (`seal.go:76-77,144-146`);
    - USTAR needs the size up front;
    - authorize needs the exact ciphertext size (`controlplane/uploads.go:36-41`).
  - Use a two-pass deterministic scrub: pass 1 hashes and counts, pass 2 streams into seal and a ciphertext spool.
  - Rejected: a plaintext spool (plaintext at rest), and a format change.
  - Fail closed on any mid-stream error.
  - Worked example:
    - Today: raw bytes in flight are capped at 512 MiB and "held a few times over (staged, scrubbed, sealed, zstd/age buffers), about 2 GiB" (`memstat.go:66-68`), under a 3 GiB soft limit (`memstat.go:68`).
    - Target (illustrative, to be confirmed with `make perf`): peak RSS independent of file size, bounded by the largest line plus fixed zstd/age windows, so a 1.5 GiB Codex rollout ships under the same 3 GiB soft limit with no budget change.
- **Evidence:**
  - agentsview caps sources at 512 MiB (`docs/hosted-raw-sync.md:191`); no competitor streams end to end.
  - Entire's sharding is for CPU, not memory (`redact.go:730-736`).
- **Constitution fit:** No conflict. The spool needs a named write-path owner (`ARCHITECTURE.md:107-110`).
- **Value:** 3/5 · **Effort:** XL
- **Risks:**
  - CPU doubles.
  - A multi-hundred-MB single line still needs a bound.
  - Disk-full must fail loudly.
  - Show `make perf` RSS before and after, with no budget widening. Revisit after A14.

### A18. Cloud collector install for vendor-hosted agents
- **Problem:**
  - Codex cloud, Claude Code on the web, Copilot cloud agent and Devin never write to the laptop.
  - The Compliance API excludes cloud sessions (N:05:666).
- **Proposal:**
  - Implement the reserved `cloud_pull` (`source-spec.schema.json:43`, `gather.go:276`) only on a headless install in customer infrastructure.
  - The org vendor token lives in the machine-owner local layer and is never served.
  - Each vendor is a compiled source that writes native JSON verbatim, scrubbed and sealed.
  - Interim: if `claude --teleport` lands web sessions in `~/.claude/projects` (UNVERIFIED), document it.
  - Worked example, Codex cloud (every vendor command below is UNVERIFIED):
    1. A headless install runs in the customer's k8s cluster, enrolled with a grant like any laptop.
    2. The machine-owner layer holds the org's Codex token; the served config only enables `codex-cloud-tasks`.
    3. Hourly, the `cloud_pull` source lists tasks (e.g. `codex cloud list --json`), fetches each new task's native JSON, and writes it to a local spool keyed by task id.
    4. The normal pipeline scrubs, seals and ships each task as `mirror/source=codex-cloud-tasks/<hmac>.age`; a task that grows re-ships like a growing file.
- **Evidence:** Devin v3 messages API, Copilot streaming, `codex cloud list --json`, `claude --teleport`: all UNVERIFIED, none has a source in the notes.
- **Constitution fit:** Art. 1 and 2. An org-wide vendor token on one install is a new trust shape, so file a Constitution candidate first.
- **Value:** 3/5 · **Effort:** XL
- **Risks:** Vendors may expose no transcript export. A high-value secret sits on one container.

### A19. Keychain-backed identity unit
- **Problem:**
  - The age identity and `name_key` live in a plain 0600 `identity.json` (`src/internal/identity/identity.go:27-28,48-55`).
  - `name_key` is what keeps object names from being a confirmation oracle (`naming.go:95-97`; the `NameKey` comment in `identity.go`). Any process running as the user can read it.
- **Proposal:**
  - On macOS and Windows, store the identity unit in the OS keychain / credential store, keeping `identity.json` as a non-secret pointer. Linux keeps the file.
  - Example: `identity.json` holds `{"identity_schema":2,"install_id":"3f9e...","secret_ref":"keychain:quesma-shipper/3f9e..."}`; the age identity and `name_key` sit in the keychain item.
- **Evidence:** Trajectory keeps credentials in the OS keychain (N:09:78).
- **Constitution fit:** Art. 4, 5. An on-disk format change (AGENTS.md style rule 5): needs a migration and a human check.
- **Value:** 2/5 · **Effort:** M
- **Risks:**
  - Keychain prompts under a LaunchAgent or service account; the daemon must never block on UI.
  - Backup and restore of the identity get harder, and losing it forks the install's object names.

## Feature ideas

### F1. Retention-risk warnings and a first-sync summary
- **Problem:**
  - Claude Code deletes transcripts after `cleanupPeriodDays` (default 30). A long pause or a parked file loses the race unseen.
  - Nothing reads the setting; grep finds only `claude-code.yaml:9-11`.
  - `login` prints one line (`src/internal/cli/login.go:57`).
- **Proposal:**
  - After login, print a backfill summary, e.g. "Found 1,243 files (2.1 GB) for Claude Code and Codex. 212 are older than 25 days; Claude Code deletes after 30. Shipping oldest first."
    - Oldest-first ordering exists (`gather.go:339-345`).
    - The service already starts within 5 s of login (`src/internal/cli/run.go:69,155-190`).
  - `status`/`doctor` warn when the oldest pending or parked file is within 3 days of cleanup. Read the setting from the already-collected `~/.claude/settings.json` (`claude-code.yaml:77-96`).
  - `pause 24h` warns when it would outlast the window.
  - Managed settings: the CTO asks to "set `cleanupPeriodDays` in managed settings and confirm it holds" (N:outline/review-cto.md:74). Ship an MDM snippet (e.g. `{"cleanupPeriodDays": 90}` in Claude Code's managed settings file) plus a `doctor` check that reports the effective value and where it came from. Whether managed settings override the user value is UNVERIFIED; test it on a real machine first.
  - Step 2: sealed heartbeat counters `reaped_before_ship` and `oldest_pending_age` (heartbeat_version bump, `health.go:103`).
- **Evidence:**
  - 170 claude-code issues mention `cleanupPeriodDays` (N:05:479-481).
  - Trajectory default retention is 30 days (`docs/CONFIGURATION.md:420`).
  - Swarmia cannot backfill (N:05:398).
- **Constitution fit:** Art. 7. Reads agent settings, never edits them; the MDM snippet is pushed by admins, not Shipper.
- **Value:** 4/5 · **Effort:** S (warnings), M (counters)
- **Risks:**
  - Managed or project settings may override the value (UNVERIFIED).
  - Codex does no TTL deletion of rollouts (`codex.yaml:9`), so the warning is Claude-only there. Cursor retention is UNVERIFIED.
  - A reaper delete must be told apart from a user delete or a Codex archive rename.

### F2. MDM compliance verdict
- **Problem:**
  - `status --json` is a hidden flag (`src/internal/cli/status.go:22-23`).
  - Intune `Detect.ps1` checks installed + enrolled + service only (`src/packaging/windows/intune/Detect.ps1:15-21`).
  - The macOS guide has Jamf/Kandji install audits (`src/packaging/macos/mdm/README.md:51-65`) but no status-based extension attribute, and still says "Agent Statement" (`:28,99`).
- **Proposal:**
  - Unhide it and add a schema-versioned `compliance` object: `{"schema":1,"state":"collecting","reason":"","next_step":""}`.
  - States: collecting, not_enrolled, paused, service_down, config_expired, no_agents, failing, update_failed.
  - macOS: ship `jamf-ea.sh` and a Kandji audit that run as the console user (`launchctl asuser`).
  - Intune: keep `Detect.ps1` as installed + enrolled; keying it on `collecting` would make Intune reinstall paused machines. Add a separate Proactive Remediation pair.
  - Fix the stale wording.
- **Evidence:** Trajectory `managed status|reconcile|repair` with `reason`/`next_step` (`docs/MANAGED-ENDPOINTS.md:75-86`).
- **Constitution fit:** Art. 1.
- **Value:** 4/5 · **Effort:** S
- **Risks:** Reporting `paused` exposes a developer's opt-out to IT (Q12). It becomes a public contract before 1.0.

### F3. Secrets report from manifests (rotation worklist)
- **Problem:**
  - CISO: a credential found on the first scan must be rotated, not only redacted (N:outline/review-ciso.md:40,53).
  - `rule_hits` already ship in every sealed manifest (`manifest.go:42,73-77`), but nothing aggregates them.
- **Proposal:**
  - `quesma-reader report secrets` reads only manifest prefixes and aggregates per install, source and path, with first and last `sealed_at`.
  - By default it counts credential rules only, excluding `path-user`, `generic-entropy` and PII.
  - Example row: `install=Rafal laptop, path=/Users/__USER__/.claude/projects/-src-billing/9a2e.jsonl, rule=aws-access-key-id, hits=3, first_seen=2026-08-14T09:30Z`.
  - The report states that account objects are unscrubbed and so absent.
- **Evidence:** agentsview `secret_findings`, `secret_leak_count`, `--has-secret` (`docs/session-api.md:1088-1114`), local only.
- **Constitution fit:** Art. 2, 4. No protocol change.
- **Value:** 4/5 · **Effort:** S (after A3)
- **Risks:**
  - Hits carry no value fingerprint by design (`matcher.go:19-24`), so "first seen" is per (path, rule), not per secret.
  - Hits repeat across versions of the same file.

### F4. Unhide `log` as the developer's sent receipt
- **Problem:**
  - Developers cannot see what already left their machine.
  - The hidden `log` (`src/internal/cli/internal.go:104-140`, hidden via `cli.go:74-78`) reads audit entries that store rule_hits and the object key, but prints neither (`internal.go:129-133`).
- **Proposal:**
  - Unhide it as `quesma-shipper sent [--since 7d] [--repo .]`.
  - Print agent, repo (from the tracking attributor, `src/app/survey.go`), time, bytes, rule counts and key suffix.
  - Unhide `preview` too.
  - Example row: `claude-code  acme/api  2026-09-28 17:04  1.2 MB  email x3, github-pat x1  source=claude-code-transcripts/9f2c...age`.
- **Evidence:** vibe-log privacy preview (`src/lib/ui/privacy-preview.ts`). Datadog Lapdog's local view as an adoption hook is UNVERIFIED (no source in the notes).
- **Constitution fit:** Art. 7.
- **Value:** 3/5 · **Effort:** S
- **Risks:** Audit-log retention bounds history. Cursor has no repo attribution yet.

### F5. Content-free support bundle
- **Problem:** Diagnostics are spread across hidden commands (`cli.go:74-78`, `doctor.go:52-53`).
- **Proposal:**
  - `quesma-shipper doctor --bundle` collects:
    - doctor JSON;
    - `config --with-provenance`;
    - the last N audit entries, with HMAC leaves and `__USER__`;
    - state counts, the crash journal and build info;
    - never payload bytes.
  - It prints the contents first so the developer can review them, then seals to the existing served recipients (no new field).
  - Example: "12 entries, 0 payload bytes, paths hashed; readable by 3 recipients (includes Quesma while allow_quesma_etl is on)".
  - Remote request from FM is step 2. It needs a new upload key shape (`fleet-manager/src/upload.go:95-96`) and a locally refusable rulebook row.
- **Evidence:** Trajectory `flare`: "review the ZIP before sharing" (`docs/USER-GUIDE.md:145`). Do not copy its `--privileged` raw mode.
- **Constitution fit:** Art. 2, 4, 5.
- **Value:** 3/5 · **Effort:** S (local), M (remote)
- **Risks:** Audit text may carry repo names. The remote trigger is a new egress lever.

### F6. Release rings, holds, and central autoupdate-off
- **Problem:**
  - There is a single `release.json` target (`src/packaging/common/update.go:134-147`).
  - FM cannot even switch autoupdate off, because its authored-YAML allowlist omits it (`fleet-manager/src/model.go:268-273`), although the rulebook allows it (`resolve.go:253-259`).
  - Self-update always reaches `updates.quesma.dev`; `SHIPPER_NO_SELFUPDATE` is the only way off (`fleet-manager/OPERATIONS.md:85`). HN reader: "can I run the whole path with no Quesma account" (N:outline/review-hn-reader.md:52).
- **Proposal:**
  - Step 0 (S): add `autoupdate` to the FM allowlist.
  - Then:
    - ring targets `release-stable.json` / `release-canary.json`;
    - served `release: {ring: canary}` or `{hold: 0.4.2}` (hold back only, never force);
    - a signed TUF minimum-version floor;
    - a customer-hosted TUF mirror as the update source, so the whole path runs without Quesma infrastructure (whether the client can take a configurable update base URL today is UNVERIFIED);
    - a convergence view, e.g. "stable 0.4.2: 188/200 converged, 9 waiting for recycle, 3 stuck on 0.4.0".
  - The convergence counts need no new storage: every request carries `X-Shipper-Version`, which FM already keeps per install in seen records (`fleet-manager/README.md:181-184`).
- **Evidence:** Trajectory pinned desired version, rollback, `update converge` (`docs/USER-GUIDE.md:246-253`), convergence heartbeat (`docs/METRICS-REFERENCE.md:1457-1462`).
- **Constitution fit:** Art. 1, 3. TUF trust is unchanged; a hold is a new rulebook row.
- **Value:** 4/5 · **Effort:** M (step 0 is S)
- **Risks:**
  - Only the failure reason ("self-update failed") needs more. A failed self-update is recorded locally through `app.RecordUpdateFailure` (`src/internal/cli/update.go:177`, `src/app/judge.go:248-249`) and reaches FM only via forwarded telemetry or the reporter path, so that column depends on F7.
  - macOS all-users installs stay MDM-managed.

### F7. Install states in the admin UI from seen records and reporter health
- **Problem:**
  - FM already has the data, but the admin UI does not show it:
    - seen records hold version, OS, last config fetch and last upload authorization per install (`fleet-manager/README.md:181-186`);
    - a HealthRecord store exists, written by a separate reporter credential (`fmr1.`) that "may report collection health and nothing else" (`fleet-manager/src/health.go:1-7`, `admin_http.go:204-206,214-240`, `fleet-manager/OPERATIONS.md:231-234`);
    - the admin UI "no longer shows it; the dashboard does" (`admin_http.go:501-503`).
  - No reporter exists in this repo.
- **Proposal:**
  - Derive states from seen records alone, no new storage: `never_configured`, `configured_never_vended`, `not_seen` (e.g. 48 h), and `ok`.
  - Join reporter health, when a reporter exists, for `failing` (e.g. consecutive_failures >= 3) and failure reasons.
  - Add a count bar and filters: "212 installs: 180 ok, 14 not seen, 9 never vended, 6 failing".
  - Optional org webhook on transitions, carrying ids only by default.
  - A storage-hostname mismatch shows as vended with PUT faults, not as never vended (`fleet-manager/OPERATIONS.md:25-29`).
  - Not proposed by default: storing the forwarded cleartext `install_health` event. FM forwards it and deliberately keeps nothing (`src/app/telemetry.go:4-6,40-49`; 403 without a collector, `fleet-manager/src/telemetry.go:146-149`). Storing it reverses that choice, so it is an explicit decision (Q18).
- **Evidence:**
  - Trajectory convergence and capture-gap metrics (`docs/METRICS-REFERENCE.md:1348-1356,1457-1462`).
  - agentsview per-device `last_seen_at` (`docs/hosted-raw-sync.md:383`).
- **Constitution fit:** Art. 1; Art. 2 untouched.
- **Value:** 4/5 · **Effort:** S (seen-record states), M (with reporter health)
- **Risks:**
  - Laptops that are off read as not_seen, so label accordingly.
  - A vend is not landed bytes.
  - The webhook is new egress.

### F8. Opt-in cleartext coverage counters
- **Problem:** "Is it capturing every agent on 200 laptops" is answerable only from the sealed heartbeat (`src/internal/engine/health.go:43-79`).
- **Proposal:**
  - An hourly `install_coverage` event, stored at `control/coverage/<id>.json`.
  - A closed per-family schema in shipper-protocol: `{state, agent_version, shipped, parked, failed, oversize, unreadable}`.
    - Counts are bucketed (0, 1-10, 11-100, 100+).
    - No paths, no rule ids.
    - Org opt-in.
  - FM renders an agent x state view, e.g. "Codex: 140 collecting, 12 agent_absent, 3 root_present_no_match (all on 0.9.4)". That surfaces vendor format drift on day one.
- **Evidence:** Trajectory capture-health metrics (`docs/METRICS-REFERENCE.md:1337-1360`); agentsview failure classes (`docs/hosted-raw-sync.md:409-418`).
- **Constitution fit:**
  - Art. 2: counts, not bytes. A chosen disclosure per `src/app/telemetry.go:4-6`.
  - It turns a forward-only route into a stored one: protocol review.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Per-install activity next to hostnames approaches monitoring; bound it by N4.
  - Versioned-bucket churn (`fleet-manager/src/seen.go:14-16`).

### F9. Volume and metering ledger
- **Problem:**
  - No measured GB per developer per month exists (N:outline/PROPOSED-OUTLINE.md:301).
  - There is no pricing unit, and no data to decide A14.
  - CTO: "measure it with `preview` first" (N:outline/review-cto.md:54); HN reader asks how long `preview` takes (N:outline/review-hn-reader.md:54).
- **Proposal:**
  - Step 0 (S), local and before any FM ledger: `preview` prints a volume projection and its own runtime, e.g. "Claude Code 41 MB/day, Codex 12 MB/day over the last 14 days (projected from file mtimes and sizes); preview took 3.2 s".
  - Aggregate authorize requests (size, source-id, agent-version; `fleet-manager/src/upload.go:48-54`) per install per UTC day. Write one Create at `control/usage/<yyyy-mm-dd>/<install>.json`, excluding heartbeat objects (`upload.go:96,108`).
  - Compute the re-ship ratio from FM's own key record. It is a metric only and never feeds `already_present` (A2).
  - UI + CSV, e.g. "acme, week 39: 212 active installs, 41.3 GB authorized, 38% of bytes re-ship an existing key".
- **Evidence:** CTO review asks for the number (N:outline/review-cto.md:24,54).
- **Constitution fit:** Art. 2 (FM already sees sizes), Art. 7.
- **Value:** 4/5 · **Effort:** S (local projection), M (ledger)
- **Risks:** Authorized is not landed. A multi-instance FM needs per-instance partial keys. The local projection ignores re-ship multiplication.

### F10. Config change safety: validate, diff, audit, echo
- **Problem:**
  - FM checks only allowed keys and YAML syntax (`fleet-manager/src/model.go:261-274`).
  - A value the client refuses is found per laptop, which falls back to cache (`src/app/resolve.go:22-23`), and the admin sees nothing.
  - No audit trail; one admin credential.
  - shipper-protocol v0.1.0 has no served-config document schema. It ships only config-request/response, enroll and v2 upload schemas, plus the `authority.json` rulebook.
- **Proposal:**
  - Step 1: publish a served-config document schema in a shipper-protocol release. FM's rule is that a "new shipper-facing behaviour starts in `shipper-protocol`" (`fleet-manager/ARCHITECTURE.md:74-75`), and FM is one `package main` (`fleet-manager/ARCHITECTURE.md:4-6`) that shares code only through that module (`fleet-manager/go.mod:12`).
  - Validate against it before apply (catches typos such as `enabeld`).
  - Diff the rendered config.
  - Write a Create-only `control/audit/<ts>-<rand>.json` carrying the full before and after bodies. FM lacks `GetObjectVersion`, so versions cannot be read back (`terraform/aws/main.tf:128-136`).
  - The client echoes the applied etag or a refusal reason into the seen record, e.g. "181/200 on config v14, 2 refused: roots outside catalog".
- **Evidence:** Trajectory strict YAML and `config sync`/`reload` (`docs/CONFIGURATION.md:187-203`).
- **Constitution fit:** Art. 1, 3. The schema and the echo are protocol additions.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - A JSON schema cannot express every resolver rule; full parity needs resolver logic in shipper-protocol.
  - Validate against the oldest client version still seen.

### F11. Private sessions and `pause --exclude`
- **Problem:**
  - `pause 1h` before a sensitive session only delays it.
  - The pause file records At/Until (`src/internal/platform/pause.go:18-39`), and on resume the changed file ships whole (`engine/file.go:57-63,85-111`).
  - Cursor ignores `.notrajectories` (`ignore.go:61`).
  - There is no in-agent surface.
- **Proposal:**
  - A local never-ship ledger keyed by state key.
  - `quesma-shipper private [--session <path>]` and `pause --exclude 1h` add the files active in the window. Later appends stay excluded, and audit and status say `skipped: private (paused 14:02-15:02)`.
  - Bytes shipped before the pause stay in the bucket; the CLI says so.
  - Plain `pause` keeps meaning defer (Q5).
  - Cursor coverage via its workspace slug (`cursor.yaml:26-43`).
  - In-agent without a plugin: honour in-content markers. A prompt containing `<private>` (claude-mem, N:09:164) or `<sensitive>` (Trajectory, N:09:77) puts the file on the ledger, or redacts the marked span. Example: a developer types `<private>customer escalation for client-x</private>` and the session never ships.
- **Evidence:**
  - Trajectory has three scopes: user-wide `capture disable`, whose running servers discard events immediately (`docs/PRIVACY.md:73-84`); per-session `/incognito`, which keeps local JSONL capture (`plugin/trajectory/skills/incognito/scripts/toggle.sh:445-447`); per-process `TRAJECTORY_DISABLED=1` (N:09:75).
  - Its skill maps "stop recording" to incognito, which keeps local capture (`skills/incognito/SKILL.md:39`). That is the anti-pattern.
  - pond erase denylist.
- **Constitution fit:** Art. 7. Consistent with "remote cannot undo a local disable" (`resolve.go:322-341`).
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - mtime-in-window over-excludes long sessions. This is deliberate and loud.
  - Clock changes can shift the window.
  - Marker detection reads content on the laptop; it is a string match, not a model (N1). Span-only redaction must fail closed on unbalanced tags.
  - An in-agent `/no-ship` skill still needs a plugin surface Shipper lacks.

### F12. Retention and legal hold in the Terraform templates
- **Problem:**
  - The only lifecycle rule expires `lifecycle=ephemeral` control records, on AWS only (`fleet-manager/terraform/aws/main.tf:47-62`). GCS has none (`terraform/gcp/main.tf:50-56`).
  - This is the top CISO row (N:outline/review-ciso.md:27,38).
- **Proposal:** Add Terraform variables:
  - `mirror_noncurrent_days`:
    - AWS: filter on the `class=trajectory` tag that FM already puts on every mirror PUT (`fleet-manager/src/upload.go:173`; heartbeats get `class=context`, `upload.go:179`). Tickets carry a signed `x-amz-tagging` header (`fleet-manager/OPERATIONS.md:95-96`) and FM checks the ticket's required headers against the validated object (`fleet-manager/src/server.go:424-433`), so this is a Terraform-only change.
    - GCS: `matchesSuffix: .age`.
  - `mirror_retention_days`, off by default.
  - `legal_hold_mode = none|governance|compliance`. A separate operator tool applies it under its own principal, never FM's runtime role; FM does no deletes or holds (`fleet-manager/ARCHITECTURE.md:80-81`).
  - Example: `terraform apply -var mirror_noncurrent_days=14 -var legal_hold_mode=governance`.
- **Evidence:** Trajectory `retention_days` (`docs/CONFIGURATION.md:420`); Compliance API 6 years (N:09); OpenAI 30 days (N:05:688).
- **Constitution fit:** Art. 2 and 5 unaffected. Grant changes need confirmation.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Expiry deletes the history that F16 needs.
  - Compliance mode is irreversible and blocks erasure, so default to governance.

### F13. Per-session erasure with served tombstones
- **Problem:**
  - The only erasure unit is the install prefix (`naming.go:105-106`), and FM has no delete (`README.md:641`).
  - A deleted object comes back on the next append.
- **Proposal:**
  - A separate operator tool, not FM (FM ARCHITECTURE: a delete "is not a fleet-manager change. It is a constitutional one", `fleet-manager/ARCHITECTURE.md:80-81`). Working name: `shipper-ops erase --org acme --leaf ab12...`, under its own `DeleteObjectVersion` credential. It deletes all versions and appends the leaf to `control/erased/<install>.json`.
  - FM serves `erased: [...]` read from that record. The shipper skips matching `MirrorName`s (`skipped: erased by organization`).
  - FM only ever sees HMAC leaves (`name_key` is client-side).
  - An identity reset mints a new `name_key`, so also add the canonical path to the F11 ledger.
  - File a Constitution candidate for the delete path first (`CONSTITUTION.md:65-68`).
- **Evidence:** pond `erase` + denylist (`docs/spec.md:506`); no coding agent offers per-transcript deletion (N:05:575).
- **Constitution fit:** Art. 1, 2, 7. Needs a narrow-only served-schema row and a Constitution candidate.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - The laptop copy remains; say so.
  - The list needs compaction.
  - The delete credential must never reach the FM runtime.
  - Session-level erasure needs A11.

### F14. Reader transparency and custody attestation
- **Problem:**
  - `preview` prints bare age1 recipients (`src/internal/cli/print.go:23`), so developers cannot tell Quesma's default-on key from a custodian.
  - Total key loss is silent (`README.md:145-147`).
- **Proposal:**
  - Served recipient labels `{recipient, label, kind: custodian|etl|quesma}`.
    - `kind: quesma` is server-derived (`fleet-manager/src/server.go:275-277`); the others are admin-authored.
    - Show the key id next to each label.
  - Status and preview print, e.g., "Readable by: 2 Acme custodians, Acme ETL, Quesma (allow_quesma_etl)".
  - Custody proof:
    - every 90 days FM seals a nonce to each recipient;
    - the custodian answers with `age -d -i key.agekey canary.age | fleet-manager-cli custody prove`;
    - the UI shows "acme-sec-2 NOT proven for 97 days".
- **Evidence:** Trajectory `config show` / `publish status` (`docs/PRIVACY.md:94-102`).
- **Constitution fit:** Art. 1, 4. Labels are display-only.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Labels can lie.
  - Person names in labels are personal data.
  - The proof route needs rate limits.

### F15. Headless and CI installs
- **Problem:**
  - `claude -p` and `codex exec` containers die within minutes, so the poll never sees them.
  - FM neither groups nor expires ephemeral installs.
- **Proposal:**
  - `QuesmaOrg/shipper-action`:
    - enrolls a per-job install with `SHIPPER_AUTH_KEY` (`README.md:618`);
    - runs the agent;
    - posts `quesma-shipper run --once --drain`;
    - fails the job when drain returns `complete=false` (`src/app/app.go:221-223`).
  - CI context (`GITHUB_REPOSITORY`, `GITHUB_RUN_ID`, PR) goes into a sealed context object.
  - FM marks installs `kind: ephemeral`, groups them by grant, and expires them.
  - Example (the action and its inputs do not exist yet):
    ```yaml
    jobs:
      agent:
        runs-on: ubuntu-latest
        steps:
          - uses: actions/checkout@v4
          - uses: QuesmaOrg/shipper-action@v1
            with:
              auth-key: ${{ secrets.SHIPPER_AUTH_KEY }}
              fleet-manager: https://fm.acme.internal
          - run: claude -p "fix the flaky login test" --permission-mode acceptEdits
          # post step: quesma-shipper run --once --drain; fails the job if complete=false
    ```
- **Evidence:**
  - Agent SDK "always writes ... to local disk first", with SessionStore mirrors because "Local containers are ephemeral" (N:05:363-370).
  - memento GitHub Action.
- **Constitution fit:** Art. 1. Art. 5 needs one install per job, because a shared identity collides on path-keyed objects.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Control-record growth.
  - Long-lived grant secrets in CI.
  - Stored-record changes need sign-off (`fleet-manager/AGENTS.md`).

### F16. Forensic history across bucket versions
- **Problem:**
  - Agents have tried to wipe their own transcripts and to spoof tool calls (N:07:67-72).
  - The client notes a shrinking file locally and ships it anyway (`src/internal/engine/file.go:107-111`).
- **Proposal:**
  - `quesma-reader history --session 4f1c` classifies each version transition as append, rewrite or shrink, e.g. "v18 12.3 MB SHRINK: diverges at line 1,044, prior content retained in v17".
  - `verify` recomputes `shipped_hash` (`seal.go:216-220`).
  - Docs state the exact claim: a versioned bucket, a write-only runtime (`fleet-manager/OPERATIONS.md:13-18,141-143`) and client-side hashes.
- **Evidence:** Tapes `raw equivalence`; agentsview parent-receipt manifests fail closed on rewrite.
- **Constitution fit:** Art. 2, 5.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - Needs `s3:GetObjectVersion`, which the shipped grant forbids (`terraform_test.go:15`), so a customer-held role.
  - Valid only if the scrub config did not change between versions (A1).
  - A compromised laptop can still upload a rewrite.
  - Conflicts with F12 expiry.

### F17. Export bridges, agentsview layout first
- **Problem:** Buyers want search and cost UIs now ("a customized interface", N:05:161).
- **Proposal:**
  - `--layout agentsview` writes `<dest>/<install-name>/raw/claude/<project>/<uuid>.jsonl` to a local dir or a customer S3 prefix that agentsview reads directly.
  - Then OTel GenAI spans (content off by default), then Langfuse.
  - Refuse non-customer endpoints unless explicitly flagged.
  - Example: `quesma-reader export --since 14d --layout agentsview --to s3://acme-view/raw-archive/`.
- **Evidence:**
  - agentsview `s3://` roots for Claude, Codex and Cursor (`docs/configuration.md:1256-1332`).
  - Trajectory `otlp` and `local_forward_url` (`docs/CONFIGURATION.md:240-242`, `docs/USER-GUIDE.md:269-283`).
  - Langfuse Claude Code integration (N:09:141).
- **Constitution fit:** Art. 2, 3, 4.
- **Value:** 4/5 · **Effort:** M
- **Risks:**
  - agentsview ships weekly minors.
  - Cursor `.enriched.jsonl` is Shipper-specific.
  - `__USER__` appears in payload bytes, not just paths (`scrub.go:706`).
  - agentsview is also a competitor.

### F18. Second wave of file-based agents
- **Problem:**
  - Copilot CLI, Gemini CLI, OpenCode and Pi are collected by Trajectory, SpecStory, pond and agentsview.
  - Copilot is the largest enterprise gap.
- **Proposal:** Four compiled `file_glob` families:
  - `copilot-cli-sessions`: `~/.copilot`, `session-state/**/*.jsonl`.
  - `gemini-cli-chats`: `~/.gemini`, `tmp/*/chats/session-*.json[l]`. `sniff: json` covers both, since it checks only the first byte (`gather.go:615-619`).
  - `opencode-storage`: `~/.local/share/opencode`, `storage/{session,message,part}/**`.
  - `pi-sessions`: `$PI_CODING_AGENT_DIR`, `~/.pi/agent`, `sessions/**`.
  - Next candidate: Continue's `.continue/dev_data` local file store (N:09:159).
  - Each family needs:
    - an exemption baseline;
    - cwd fields (A5);
    - deny entries after a real-machine root audit;
    - fixtures;
    - a `docs/format-provenance.md` row.
  - Decide the reserved `acp` primitive. It is reserved next to `cloud_pull` and absent by construction (`source-spec.schema.json:43`, `gather.go:276`), with no stated semantics. If it means capture over the Agent Client Protocol (UNVERIFIED), it needs a listener or a launcher, which N2 rules out; either write down what it is for or drop the reservation.
- **Evidence:**
  - agentsview discovery (`internal/parser/types.go:245-295,534-542`; line numbers UNVERIFIED).
  - Trajectory and SpecStory lists (N:09:41,60).
- **Constitution fit:** Art. 1 (compiled, enabled centrally), Art. 4.
- **Value:** 5/5 · **Effort:** L
- **Risks:**
  - Gemini and Copilot credential locations are UNVERIFIED.
  - Current OpenCode may keep sessions in `opencode.db` (agentsview `discovery.go:154`); report partial coverage.

### F19. Hooks as wake signals only
- **Problem:**
  - Up to 15 min latency (`schedule.go:9-13`) and no session-end signal.
  - A laptop closed right after a session waits for the next boot.
- **Proposal:**
  - `quesma-shipper wake` touches `<state_dir>/wake/<hmac(transcript_path)>` and exits well inside Claude's 1.5 s SessionEnd budget.
  - The daemon flushes only those paths:
    - it coalesces Stop wakes, which fire every turn;
    - it always honours SessionEnd;
    - it re-stats, since `transcript_path` "may lag".
  - Signal the daemon rather than readdir every few seconds (`schedule.go:12` calls a seconds poll a hot loop). Files stay authoritative and polling remains the backstop.
  - Distribute it as a snippet admins push via managed settings, e.g. `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"quesma-shipper wake","timeout":1}]}]}}`.
- **Evidence:**
  - Trajectory "wake hooks + authoritative transcript/DB reconciliation", with least-invasive setup and owned-only uninstall (`docs/CLIENT-INSTRUMENTATION.md:36-42,146-197`).
  - Entire's stale matchers "silently never fired".
  - code.claude.com/docs/en/hooks.
- **Constitution fit:**
  - Art. 7 unaffected.
  - A Shipper-run installer would break `ARCHITECTURE.md:107-110`. It would also edit `~/.claude/settings.json`, which Shipper itself collects (`claude-code.yaml:77-97`).
- **Value:** 3/5 · **Effort:** M
- **Risks:**
  - Frequent wakes multiply whole-file uploads until A14.
  - Managed settings carrying hooks is UNVERIFIED.
  - A `session_end_reason` manifest field would be a protocol change.

### F20. Claude Code raw API bodies source
- **Problem:** Transcripts omit the effective system prompt, injected `CLAUDE.md`, skills and tool schemas.
- **Proposal:**
  - A compiled, default-off `claude-code-api-bodies` source over the directory Claude Code writes with `OTEL_LOG_RAW_API_BODIES=file:<dir>`:
    - `index.jsonl` (session_id, model, request_id, message_uuid, request_file, response_file), append-only, and only on Claude Code v2.1.274+;
    - `<uuid>.request.json`, written once per attempt;
    - `<request_id>.response.json`.
  - Ship the request/response files once each (retries produce new files). Treat `index.jsonl` as a growing file: under whole-file re-ship (`engine/file.go:107`) it re-ships whole on every changed tick, so it is the first candidate for A14's quiescence gate or segments.
  - FM docs give an MDM env snippet with an absolute path. Shipper never sets the variable itself.
  - Worked example (values illustrative): one index line
    `{"session_id":"4f1c...","model":"claude-opus-4-...","request_id":"req_01A...","message_uuid":"9b2e...","request_file":"9b2e....request.json","response_file":"req_01A....response.json"}`
    produces two new objects, `mirror/source=claude-code-api-bodies/<hmac(request)>.age` and `<hmac(response)>.age`, plus a new version of the `index.jsonl` object.
- **Evidence:** code.claude.com/docs/en/monitoring-usage (fetched 2026-09-29; thinking is redacted; no cleanup of the directory is documented).
- **Constitution fit:** Art. 1, 4.
- **Value:** 3/5 · **Effort:** M
- **Risks:**
  - Bodies repeat the full context, so measure volume first (F9 step 0).
  - No cleanup is documented, so the directory grows without bound on the laptop; say so in the MDM snippet docs.
  - Bodies may exceed the caps.

### F21. Claude-grammar siblings
- **Problem:**
  - Qwen Code, OpenClaude, iFlow, Qoder, WorkBuddy and Claude Desktop local agent mode write Claude-style `projects/` trees.
  - The catalog already notes nine agents share the grammar (`claude-code.yaml:1-4`).
- **Proposal:**
  - One source per sibling with its own `source_id` in family `claude-code`, so Claude exemptions apply (`exemptions.go:9`).
  - Add each to the cwd probe (`ignore.go:61`) so `.notrajectories` holds.
  - Deny auth files after a root audit.
  - Example: `{id: qwen-code-transcripts, roots: [$QWEN_HOME, ~/.qwen], require_subdir: projects}`.
  - Start with the two most used.
- **Evidence:** agentsview README roots (`README.md:353-455`, line range UNVERIFIED); SpecStory lists Qwen Code (N:09:41).
- **Constitution fit:** Art. 1, 4.
- **Value:** 3/5 · **Effort:** M
- **Risks:** Byte compatibility per sibling is UNVERIFIED. A missing deny entry is the real leak.

### F22. Restore a developer's sessions from the bucket
- **Problem:** "Restore, not just collect" (N:outline/review-hn-reader.md:56). No read path exists for developers.
- **Proposal:**
  - Make it a reader command, not a shipper verb. The shipper never writes into agent stores (`writepath_lint_test.go:78`) and links no cloud SDK.
  - `quesma-reader restore --identity <key> --session 4f1c --to ~/.claude/projects/-src-api/`:
    - refuses to overwrite a newer local file;
    - sets mtime to now, because an old mtime is what the reaper deletes (N:05:536).
  - Restored bytes are post-scrub, so their hash differs from `source_hash` and they would re-ship. Add restored paths to the F11 ledger.
  - Authorization: a customer-managed read role (template precedent `etl_reader_role_arns`, `terraform/aws/variables.tf:28-32`).
- **Evidence:** pond restores into any client (`docs/spec.md:35`); Trajectory `resume --target codex`.
- **Constitution fit:** Art. 2. Art. 4: needs a key the developer holds; FM forces the install recipient off (`admin-ui/app.js:125`).
- **Value:** 3/5 · **Effort:** M (customer role), XL (FM-vended GETs)
- **Risks:**
  - FM GETs need read on all ciphertext, which `main.tf:175-181` rejects.
  - Resume fidelity is best-effort (redacted signatures; see F24 d).
  - HR policy for departing staff.

### F23. Opacity and completeness report
- **Problem:**
  - An archive that implies completeness misleads.
  - Claude full thinking exists only in an encrypted `signature`.
  - Since 2026-06-05, Codex inter-agent messages have an empty `content` (N:06:111-116).
- **Proposal:**
  - A per-session reader report of:
    - provider-sealed items;
    - Shipper redactions by rule;
    - collection gaps from sealed heartbeats (`oversize`, `unreadable`, `health.go:62-66`; per install and source only).
  - Example: "4f1c: 212 turns; provider-sealed: 38 thinking blocks, 4 Codex subagent tasks with empty content; redacted: 17; portable: partial".
  - Unknown shapes report `unknown`, never 0.
- **Evidence:** Earendil's five checks (N:06:14,52); Codex issue #28058.
- **Constitution fit:** Art. 2.
- **Value:** 3/5 · **Effort:** M (S after A8)
- **Risks:** Vendor field names drift.

### F24. Close probed scrub gaps and publish recall
- **Problem:** A single probe run (entire-specstory map) found:
  - **Misses:**
    - JDBC query-string and ADO.NET `Password=...;` forms;
    - bare lowercase `password=x` or `password: x` in free text (libpq, YAML). JSON keys whose last word is `password`, `passwd` or `pwd` are already redacted (`transforms/keyname.go:14-17`, `heuristic.go:428-433`), and a free-text `NAME=value` matcher exists (`heuristic.go:403-405`), but it runs only over configured names, whose defaults are suffix forms such as `*_PASSWORD` (`heuristic.go:363-372`).
    - URL-form DSNs are already caught (`packs/data/cloud-keys.json:29-31`).
  - **False positives:** `DB_PASSWORD=changeme`, `ghp_xxxx`.
  - **Thinking signatures:** `generic-entropy` shreds Claude `thinking.signature`. There is no exemption (`exemptions.go:7-44`), and `/` splits runs (`heuristic.go:73-80`). This breaks resume.
- **Proposal:** Separate PRs, each with a confirmed golden diff:
  - (a) A `connection-strings` pack that redacts only the password value.
  - (b) A bare credential assignment rule for free-text `password`/`passwd`/`pwd` followed by `=` or `:`.
  - (c) An exact-value placeholder allowlist that never splits a finding. It reverses the pinned AWS docs key test (`packs/quesma_extra_test.go:28-31`; Q7).
  - (d) Exempt `message.content[].signature` from the backstop only; patterns still run. Precedent: Entire skips any key ending in "signature" for Claude Code (`entire/redact/redact.go:1338-1344`); (d) is narrower, one path and backstop only.
  - (e) `make recall` over the existing adversarial tests and calibration corpus (`calibration_test.go:12-16`), printing e.g. `connection-strings recall 38/38 fp 0/112`.
- **Evidence:**
  - Entire DSN layer (`redact/redact.go:43-46,353-392`), placeholders (`:77-90`), `samples[]` (`packs.go:67`).
  - SpecStory fails open (`pkg/redact/redact.go:104-111`).
- **Constitution fit:** Art. 4.
- **Value:** 4/5 · **Effort:** L
- **Risks:**
  - New rules can shred code identifiers; (b) must not fire on `password: string` type annotations.
  - Recall is corpus-bound; word it that way.

### F25. Repo policy by git remote, plus a narrow-only repo file
- **Problem:** An org cannot say "never collect github.com/acme/client-x". `.notrajectories` is manual and per clone.
- **Proposal:**
  - Served `repos: {deny: ["github.com/acme/client-*"], allow: [...], unattributed: collect|skip}`, matched on the normalized origin (read via A7).
  - A committed `.quesma-shipper.yaml` is honoured only if:
    - it is committed at HEAD;
    - its origin is in served `trusted_origins`.
  - Closed schema:
    - it may add `secret_key_names`, add excludes, or set `collect: false`;
    - it may never enable sources, widen roots, add recipients or loosen scrub.
  - Worked example. Served config:
    ```yaml
    repos:
      deny: ["github.com/acme/client-*"]
      unattributed: collect
      trusted_origins: ["github.com/acme/*"]
    ```
    Committed `.quesma-shipper.yaml` in `github.com/acme/payments`:
    ```yaml
    secret_key_names: [PAYMENTS_HSM_PIN]
    exclude: ["**/fixtures/cards/**"]
    ```
    Result: sessions in `acme/client-bigbank` never ship; sessions in `acme/payments` ship with `PAYMENTS_HSM_PIN=...` redacted. A `.quesma-shipper.yaml` in a fork at `github.com/mallory/payments` is ignored, because its origin is not trusted.
- **Evidence:** Trajectory `allowed_origins`, `require_committed`, narrow-only (`docs/REPO-MARKERS.md:39-44,118-120`); agentsview per-target project filters (`docs/pg-sync.md:69-70`).
- **Constitution fit:** Art. 1, 3. Narrow-only rulebook.
- **Value:** 4/5 · **Effort:** L
- **Risks:**
  - Remote normalization (ssh vs https, forks).
  - The repo file is attacker-controlled.
  - Cursor falls under `unattributed` until A5.

### F26. Cost and usage rollups with honest status
- **Problem:** CTO: "does it roll up by team and repo" (N:outline/review-cto.md:28-29).
- **Proposal:**
  - A reader `usage` view over A8 tables.
  - Transcript usage is deduped by `message.id` + `requestId` (both kept by exemptions, `exemptions.go:11`) and priced from a pinned snapshot.
  - Sliced by label (A10), repo (A7) and model.
  - Every figure carries `cost_status` priced|unpriced|unavailable; unpriced never becomes 0.
  - Example: `quesma-reader usage --since 2026-09-01 --by repo,model`.
- **Evidence:**
  - Trajectory `cost_status` (`docs/LLM-OBS-SPAN-TAGS.md:42`); "a missing ratio is unavailable, not zero" (`docs/REPORTS.md:59`).
  - agentsview `usage daily --breakdown` (`README.md:207-278`).
- **Constitution fit:** Art. 2.
- **Value:** 4/5 · **Effort:** L
- **Risks:**
  - On subscription plans, token cost misleads.
  - Account endpoints likely return plan utilization, not per-model tokens (UNVERIFIED).

### F27. Eval collection builder
- **Problem:**
  - Evals are the only measured gain in the notes: LangChain went from 52.8 to 66.5 on Terminal-Bench 2.0 (N:05:101).
  - Braintrust turns failed traces into eval cases (N:05:153).
- **Proposal:**
  - Evals first: `quesma-reader dataset build --where "repo LIKE 'github.com/acme/%' AND tool_errors>=3" --out evals-2026q4.jsonl --manifest evals-2026q4.provenance.parquet`.
  - Dedup by source hash and drop injected parts.
  - Rows cite `(object_key, version_id, line range)` so erasure can find them.
  - Training export later.
- **Evidence:** Trajectory outcomes are explicitly heuristic (`docs/REPORTS.md:45-53`); pond erase denylist.
- **Constitution fit:** Art. 2.
- **Value:** 3/5 · **Effort:** L
- **Risks:**
  - Weakest buyer demand; the CISO wants the distillation framing cut (N:outline/review-ciso.md:30,43).
  - Provider training terms are UNVERIFIED.
  - Scrub sentinels would end up in the training data.

## Deliberate non-goals

Art. 3 says a hard rejection "needs an article here to stand on" (`CONSTITUTION.md:31-32`). So each of these should be filed under "Candidates under discussion" (`CONSTITUTION.md:65-68`), not as ARCHITECTURE.md notes.

- **N1. No lossy projection in place of native files, and no classification or segmentation model over session content on the laptop.**
  - Rule:
    - ship the original bytes, after scrub;
    - derived objects only beside raw, and only when the source never ships (the Cursor join);
    - no LLM or ML classification or segmentation of sessions. A future scrub model is governed by N6, not N1.
  - Word it precisely. The client already parses in bounded ways: the JSON walker, cwd probe, sniff and cursorjoin. Plaintext metadata already carries content-derived `shape-sniff`, `agent-version` and `enrich-status` (`manifest.go:165-176`).
  - Why:
    - a parser fix becomes a rederive over history, instead of a permanent loss on every laptop;
    - it avoids agentsview's parser surface (408 `.go` files in `internal/parser`);
    - Trajectory's default-on segmentation sends content to a model (`docs/LLM-CAPACITY.md:15-17`) and keeps canonical JSONL for only 30 days.
- **N2. No listener, proxy, launcher, fetch intercept or resident local UI.**
  - New capture enters only as compiled primitives that yield candidates.
  - Wording: "never sits in the agent's request path". It cannot claim "never sees auth headers": the claude-account collector sends the user's OAuth token to api.anthropic.com (`accounts_claude.go:40-75`).
  - Point developers to agentsview for local browsing.
  - Why:
    - Trajectory's 19222 capture server has no documented auth, and its incognito toggle is a plain curl POST (`toggle.sh:444-445`);
    - Trajectory also fetches plugins unpinned from `raw.githubusercontent.com/.../main/` (N:09:89), so remote code runs without a pinned version;
    - Helicone is "no longer actively developed" (N:09:147);
    - Langfuse calls hooks telemetry, not enforcement.
- **N3. Never write into customer repositories:** no trailers, notes, refs or git hooks.
  - Carve out `.notrajectories`, which `tracking` writes on a keypress (`src/internal/cli/tracking.go:26-29`).
  - Linkage is computed downstream from A7.
  - Why: Entire's shadow branches hold raw working-tree blobs, and "If your repository is public, this data is visible to the entire internet" (N:09:32,34).
- **N4. No per-developer scores, outcomes or leaderboards in Fleet Manager.**
  - People-derived metrics are computed downstream, with a minimum group size.
  - This also bounds F8 and F9 (bucketed, opt-in).
  - Rollout wording must note that Quesma is a key holder by default.
  - Why: works-council and CISO objections (N:outline/review-ciso.md:22,39).
- **N5. Local opt-outs always win; no incognito-exempt destination.**
  - Local opt-outs: pause, `.notrajectories`, a local `enabled: false`, and private sessions.
  - Why: Trajectory's `incognito_exempt` destinations "may still receive spans", including thinking (`docs/PRIVACY.md:24-34`).
- **N6. No network model decides or rewrites what ships.** Any future ML scrub is on-device, fail-closed and opt-in. This rules out copying Entire's OPF tier, which is network-backed (`redact/fingerprint.go:28-29`).

Fleet Manager's own architecture adds a constraint on the ideas above: "A change that needs a payload, a database, a lock, or a delete is not a fleet-manager change. It is a constitutional one." (`fleet-manager/ARCHITECTURE.md:80-81`). So F12's legal-hold tool, F13's erase and A13's key shred are separate operator tools under their own principals, and F13 and A13 each need a Constitution candidate before code.

## Suggested sequencing

**Now**
- A1: scrub fingerprint in the sealed manifest (golden confirmation).
- A2: run the reproduction, then drop the dedup probe.
- F1: retention warnings, login backfill summary and the `cleanupPeriodDays` doctor check.
- F2: compliance verdict, plus `autoupdate` in the FM allowlist (F6 step 0).
- F9 step 0: local volume projection in `preview`.
- A3 reader v0, with F3 secrets report (lives in `agent-statement`, see Q8; this repo's part is a public seal/manifest decode package).
- F7: install states in the admin UI from seen records.

**Next**
- A5 catalog expressiveness, then F18 second-wave agents.
- A4: keyed content hashes and `THREAT_MODEL.md`.
- F11: private sessions, `pause --exclude` and in-content markers.
- A7: repo attribution in the sealed manifest.
- F9 volume ledger, then the A14 interim quiescence gate.
- F12 retention and legal hold; F13 erasure tombstones after its Constitution candidate.
- F10 step 1: served-config schema in a shipper-protocol release.

**Later**
- A14 segment upload, chains required (Q2).
- A9 customer-signed recipient policy, before any SaaS Fleet Manager.
- A10 labels and overlays.
- A13 epoch keys, after a Constitution candidate.
- A8 derive + F26 cost rollups.
- A15 `db_export`.
- A19 keychain-backed identity.

## Open questions for the founder

1. **`allow_quesma_etl` default** (on today, `README.md:149-159`):
   - (a) keep on and disclose via F14 labels;
   - (b) off for new orgs, opt-in at org creation;
   - (c) off everywhere, with migration.
   - Recommend (b): it is the plainest CISO blocker.
   - **Decided 2026-09-29: (b).** Rejected: (a) keep on, (c) off everywhere with migration.
   - It is also a data-residency question the CISO raises (N:outline/review-ciso.md:28,36): with it on, decryption can happen in Quesma's account through the outside-ETL bucket policy (`fleet-manager/OPERATIONS.md:68-70`). Whichever option, state where Quesma's ETL runs (region, account) in F14's "Readable by" line.
2. **Growing files:**
   - (a) status quo;
   - (b) quiescence gate only;
   - (c) segments, keeping a periodic full object at the legacy key so `age -d | zstd -d | tar` keeps working;
   - (d) segments, and consumers must read chains.
   - Recommend: measure with F9, ship (b), then (c).
   - **Decided 2026-09-29: (d).** Rejected: (a) status quo, (b) gate only, (c) segments plus a legacy full object. Consequences: the `agent-statement` reader (Q8) must reassemble chains; the documented `age -d | zstd -d | tar` path (`README.md:256-262`) no longer yields a whole session for segmented sources and needs rewording; A14 moves from "later, if F9 shows a high re-ship ratio" to a planned protocol change.
3. **Agent hooks:**
   - (a) never;
   - (b) a documented snippet that admins push via MDM;
   - (c) an opt-in `hooks install` with a deny marker.
   - Recommend (b): keeps the write-path invariant.
   - **Decided 2026-09-29: (b).** Rejected: (a) never. (c) opt-in `hooks install` stays open for consideration (founder, 2026-09-29), now that segments (Q2) make frequent wakes cheaper; it would need an `ARCHITECTURE.md` exception to the write-path rule.
4. **Served new sources (A6):**
   - (a) compiled only;
   - (b) served, restricted to a compiled root allowlist, auto-activate;
   - (c) served, pending local approval.
   - Recommend (b).
   - **2026-09-29: no change yet; left open.**
5. **`pause` semantics:**
   - (a) keep defer, add `private` and `pause --exclude`;
   - (b) default to exclude, with `--defer`;
   - (c) status quo.
   - Recommend (a): no silent change to a shipped command.
   - **2026-09-29: no change yet; left open.**
6. **Claude `thinking.signature`:**
   - (a) exempt from the entropy backstop, patterns still run;
   - (b) keep redacting;
   - (c) (a) plus exempt request ids.
   - Recommend (a): preserves resume, golden change. Precedent: Entire skips every key ending in "signature" for Claude Code (`entire/redact/redact.go:1338-1344`); (a) is narrower.
   - **Decided 2026-09-29: no change yet (b for now).** (a) and (c) deferred, not rejected.
7. **Placeholder allowlist vs the pinned AWS docs key test:**
   - (a) adopt fully;
   - (b) adopt, but keep redacting known cloud example keys;
   - (c) do not adopt.
   - Recommend (b) until the reason for the pin is recalled.
8. **Where the reader lives:**
   - (a) a third module in this repo;
   - (b) shipper-protocol;
   - (c) a separate repo.
   - Recommend (a): the `internal/` import prefix, and the same release cadence as manifests.
   - **Decided 2026-09-29: none of the above.** Readers and consumption tools are simple open-source tools from Quesma research, built in the internal `agent-statement` repo. Consequence: they cannot import `src/internal/...`, so the seal/manifest decode they need must be public (a small public package, or shipper-protocol). Rejected: (a), (b), (c).
9. **Normalized schema (A8):**
   - (a) a public versioned contract;
   - (b) internal to the Quesma ETL;
   - (c) internal until two customers depend on it.
   - Recommend (c).
10. **Restore authorization:**
    - (a) a customer-managed read role;
    - (b) FM vends presigned GETs, which widens its grant;
    - (c) custodians only, on request.
    - Recommend (a), with (c) as fallback.
11. **Coverage counters at FM:**
    - (a) none;
    - (b) bucketed and org opt-in;
    - (c) exact counts.
    - Recommend (b).
12. **`paused` in the MDM verdict:**
    - (a) its own compliant state;
    - (b) folded into `collecting`;
    - (c) non-compliant.
    - Recommend (a): honest to IT without penalising developers.
13. **SaaS Fleet Manager:**
    - (a) customer-hosted only;
    - (b) SaaS with Quesma-held cross-account signing roles (A16);
    - (c) SaaS control plane plus a thin customer-side signer.
    - Recommend: if SaaS, then (c) with A9 mandatory.
    - **Decided 2026-09-29: (c), SaaS is planned; A9 is mandatory before a SaaS launch.** Rejected: (a) customer-hosted only, (b) Quesma-held cross-account roles.
14. **Cloud-hosted agents:**
    - (a) out of scope, document teleport (once verified);
    - (b) a dedicated collector (A18);
    - (c) point customers at vendor exports.
    - Recommend (a) for now.
15. **CI identity:**
    - (a) one install per job, with expiry;
    - (b) a sub-identity under a runner-pool install (protocol change).
    - Recommend (a): the clean Art. 5 story.
16. **Noncurrent retention default (F12):**
    - (a) keep everything;
    - (b) 30 days;
    - (c) a variable with no default, so everything is kept.
    - Recommend (c).
17. **Label authority (A10):**
    - (a) admin only;
    - (b) developer self-declares at login;
    - (c) the grant sets it, and admins can edit.
    - Recommend (c).
18. **Storing forwarded `install_health` in FM (F7):**
    - (a) no: states from seen records plus the reporter path only, as today's design intends (`src/app/telemetry.go:4-6`, `fleet-manager/src/health.go:1-7`);
    - (b) store a closed, bucketed subset, org opt-in (overlaps F8);
    - (c) store the full event.
    - Recommend (a) until a reporter exists; revisit with F8.

## Considered and dropped

| Idea | Status | Reason |
|---|---|---|
| Org-level skill and knowledge mining | Dropped | Demand is individual, not buyer (N:05:453,555). It deepens the monitoring concern. F17 lets customers use SpecStory `/lore` or agentsview Recall instead. |
| Protocol capability negotiation | Deferred | Reverses "No capability negotiation" (shipper-protocol PROTOCOL.md:378-380). FM already learns the shipper version without it: `X-Shipper-Version` on every request, stored in seen records (`fleet-manager/README.md:181-184`), and config-request `agent_version = platform.Current()` (`src/internal/controlplane/refresh.go:123`). That supports a server-side version-to-feature table. (The `agent-version` metadata on authorize objects is the sniffed coding-agent version, not the shipper's: `controlplane/uploads.go:52`, `src/app/vendupload.go:205`.) |
| Client sink drivers (multipart, local-dir) | Deferred | Objects are at most 512 MiB before compression (`codex.yaml:43`), below the S3 single-PUT limit (5 GB per AWS docs, not re-verified). `local-dev` covers development (`src/internal/cli/internal.go:271-275`). |
| Workspace diff snapshot | Deferred | Needs A7 and F19 first. Adds a git-exec capability class. Ships more source code. Value 2/5. |
| On-device ML PII tier | Deferred | Whole-file re-scrub per change (`engine/file.go:113`) at Entire's ~5 s per 100 KB is infeasible. Entire's OPF itself is network-backed (`redact/fingerprint.go:28-29`), so it is not a model to copy; N6 keeps the no-network-model part. |
| Served `mandatory: true` collection | Dropped | Contradicts N5 and `resolve.go:322-341`. |
| Cross-install credential `value_tag` | Deferred | Breaks the never-derive-from-value rule (`matcher.go:19-24`). FM would hold a data-relevant key. |
| Sealed per-install `findings.json.age` | Dropped | Manifests already carry `rule_hits`; F3 covers it. |
| Plaintext scrub-fingerprint header | Dropped | Plaintext metadata is a closed allowlist validated by FM (`upload/ticket.go:37-40`); the sealed manifest suffices. |
| Dedup against FM's authorized-key record (A2 option) | Dropped | A vend is not a landed PUT (`fleet-manager/README.md:190-191`); "a wrong match loses the object" (`fleet-manager/src/upload.go:222-224`). |
| TUF-verifying bootstrap installer | Mostly exists | `src/packaging/linux/install.sh:2,75-89`. macOS pkg is notarized. Windows signing is already planned (`README.md:97-101`). |
| `quesma-shipper restore` verb | Dropped | Breaks the write-path invariant; restore moves to the reader (F22). |
| Restore with mtime from `payload_mtime` | Dropped | An old mtime is what the reaper deletes (N:05:536). |
| Non-goals recorded in ARCHITECTURE.md | Dropped | Art. 3 needs an article; filed as Constitution candidates instead. |
| Delete, shred and legal-hold commands inside `fleet-manager` | Reframed | FM ARCHITECTURE forbids deletes in FM (`fleet-manager/ARCHITECTURE.md:80-81`); they became separate operator tools (F12, F13, A13). |
| "Quesma never hosts a plaintext viewer" non-goal | Reframed | Contradicts the documented Quesma ETL design (`fleet-manager/terraform/aws/README.md:150-152`); became Q1. |
| Numbered per-entity placeholders (vibe-log) | Deferred | Leaks equality within a file; needs a decision against the intent of `matcher.go:19-24`. |
| Compliance API coverage diff | Deferred | Claude Enterprise only, and ETL-side (not in this repo). |

## Unverified

- **Volume and savings**
  - Real re-ship volume, compression ratio and rebase rate for A14. No GB per developer per month figure exists.
- **Shipper runtime and code**
  - Runtime behaviour of the FM dedup probe under the AWS template (A2); the reproduction recipe is in A2.
  - That a Quesma ETL downstream parses `cwd` into a repo dimension (inferred from `exemptions.go:26-28`).
  - Whether the client accepts a configurable self-update base URL for a customer-hosted TUF mirror (F6).
  - What the reserved `acp` primitive is meant to be (F18).
- **Agent file formats and locations**
  - That Cursor agent transcripts carry a `cwd` field.
  - Credential file locations for Gemini CLI and Copilot CLI.
  - Byte compatibility of Claude-grammar siblings.
  - The agentsview README and `types.go` line ranges for agent roots.
  - Whether current OpenCode versions keep sessions only in `opencode.db`.
  - Cursor local retention behaviour. (Codex does no TTL deletion, `codex.yaml:9`.)
  - Whether managed or project settings override `cleanupPeriodDays`.
- **Claude Code**
  - Whether `~` is expanded in `OTEL_LOG_RAW_API_BODIES=file:<dir>`. (No cleanup of that directory is documented.)
  - Whether Claude Code managed settings can carry hooks.
  - What `claude --teleport` does with web sessions.
- **Scrub probes**
  - The signature and DSN results come from one probe run on one 1.17 MB fixture (entire-specstory map).
  - Why Betterleaks missed the synthetic AKIA key.
- **Trajectory**
  - Whether its launcher captures full bodies (the docs say yes; the public intercept is metadata-only).
  - Trace-level contents and what `redact_pii` covers.
  - Whether its 19222 server authenticates callers.
  - Lapdog: what it is and where it is hosted; the canonical schema.
  - Whether reconciliation later picks up sessions captured while `capture.disabled` was set.
- **agentsview**
  - Hosted Raw Sync production users.
  - Pricing source precedence.
  - The ~427k Go and ~139k Svelte/TS line counts.
- **Vendor APIs and terms**
  - End-to-end transcript export for the Devin v3 API, Codex cloud, Copilot streaming and Claude web; anthropics/claude-code#94836.
  - Copilot session streaming details ("EMU only", "REST last 48 h", `Agent-Logs-Url`): no source in the notes.
  - CloudWatch Coding Agent Insights slicing by department, team and cost center: no source in the notes.
  - Anthropic hard delete "no recovery window" (cited as N:05:661; no note contains the quote).
  - What the Claude `api/oauth/usage`, Codex `wham/usage` and Cursor usage responses contain.
  - Provider terms restricting training on outputs (N:06:78, "Not checked").
- **Other tools**
  - Tapes `transcript` ingest being live; Promptster's mechanism; Backplanes and Lore internals.
  - The Langfuse `coding-agent-tracing` URL; the memento Action modes; the vibe-log citations.
- **Business**
  - Resolved 2026-09-29: a Quesma-hosted SaaS Fleet Manager is planned, with a customer-side signer (Q13).
