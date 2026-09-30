# The shipper

What runs on each developer machine: how it works, what it collects, and how to use and configure
it. Installing and enrolling it is in the repository [README](../README.md#developer-install-the-shipper-each-machine).

## Installing

The commands are in the repository [README](../README.md#developer-install-the-shipper-each-machine). What each one does:

**macOS.** The Homebrew cask installs the command on your Homebrew `PATH` and starts a per-user
background service, which waits for enrollment. It supports macOS 13 or newer on Apple Silicon and
Intel, without administrator rights. `brew uninstall --cask quesmaorg/tap/quesma-shipper` removes
it and keeps enrollment and upload history; add `--zap` to delete local state too. The signed
`.pkg` is the same thing without Homebrew: **Install for me only** puts `Quesma Shipper.app` in
`~/Applications` and registers a launchd agent; **Install for all users** needs administrator
authorization and is updated and removed by an administrator. Before switching between the two,
uninstall the previous installation without purging local state. For Jamf, Kandji and other MDM
deployment, use the same package and a `com.quesma.shipper` managed-preferences profile: see the
[macOS MDM guide](packaging/macos/mdm/README.md).

**Linux.** The script downloads a hash-pinned bootstrap binary into `~/.local/bin` and installs a
systemd user service. Run it again to upgrade; enrollment is kept. `--no-service` skips the
service; `--from PATH` installs a local build instead of a release. Do not run it as root. The
released binaries are also available directly for
[AMD64](https://updates.quesma.dev/download/quesma-shipper-linux-amd64) and
[ARM64](https://updates.quesma.dev/download/quesma-shipper-linux-arm64).

**Windows.** Run the setup as your normal user. It installs under `%LOCALAPPDATA%`, adds the
command to your user `PATH`, and registers a scheduled task that starts immediately and at login,
without administrator rights. Re-running setup repairs that integration without changing
enrollment. For Intune, see the [Intune guide](packaging/windows/intune/README.md): user-context
installation and unattended enrollment, a platform-script route, and a Win32 app package route.
Release signing is temporarily disabled while the publisher identity is validated, so Microsoft
Defender SmartScreen may warn about the download, and devices whose application-control policy
requires a trusted publisher may block it. The raw `quesma-shipper-windows-<arch>.exe` files remain
available for portable use and are what the self-updater installs; a portable copy has no
scheduled task or uninstaller.

**Updates.** Released builds keep themselves current from Quesma's signed
[TUF](https://theupdateframework.io/) repository; `quesma-shipper update` updates at once. The
public repository and the stable download endpoints are described in
[RELEASE_DOWNLOADS.md](../RELEASE_DOWNLOADS.md). `quesma-shipper uninstall` removes the service
and program on any platform and keeps local state; `--purge` deletes enrollment and state too.

## How it works

```
agent session files ──► discover ──► detect change ──► read ──► scrub ──► encrypt ──► upload ──► commit
                        (catalog)    (size, mtime,              (rule     (age, to     (presigned  (local
                                      content hash)              packs)    org keys)    PUT)        record)
```

- **Sources.** A compiled catalog describes where each agent stores sessions and how to read them.
  This includes live SQLite databases. The shipper never writes inside an agent's store.
- **Scrub.** Every file passes through redaction rule packs first. Three provider-key packs
  (`gitleaks-core`, `quesma-extra`, `cloud-keys`), an entropy backstop (`generic-entropy`), and a
  PII pack (`pii-core`) are compiled in. The control plane can add packs. It cannot remove them.
  In the fields that carry encrypted reasoning and inline media, any string of 160 bytes or more
  is replaced whole by the `__REDACTED:dropped__` sentinel before the packs run: the entropy
  backstop almost always shredded them, and a configured exemption still wins.
  A scrub error means the file is not uploaded.
- **Encrypt.** The scrubbed file is sealed with age to the recipients that the control plane
  supplies. The client can be configured to hold no key that decrypts what it ships.
- **Upload.** The control plane authorises each upload with a presigned URL. Ciphertext goes from
  the client directly to the sink. The control plane never receives trajectory bytes. No cloud SDK
  is linked. The upload path is `net/http` only.
- **Commit.** A local record of shipped files is updated after the sink confirms the write. That
  record is the authority for resume.
- **Update.** Release builds check a [TUF](https://theupdateframework.io/) repository. They replace
  themselves when a newer signed release exists. Development builds do not self-update.

The design rules are in [CONSTITUTION.md](../CONSTITUTION.md). The code layout is in
[ARCHITECTURE.md](../ARCHITECTURE.md). The wire contract is the public
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) module. This repository imports
its schemas and fixtures. Protocol changes are reviewed and released there.

## What is collected

The shipper collects more than chat transcripts. Per agent, from the agent's own directories:

| Agent | Trajectories | Context artifacts |
|---|---|---|
| Claude Code | Session and subagent transcripts, spilled tool results | Per-project memory files, plans, todos, file-history checkpoint metadata, the user-level `CLAUDE.md`, `settings.json` |
| Codex | Sessions, one object each whether live, archived or zstd-compressed (decompressed and scrubbed like the rest), the session index | None |
| Cursor | Agent transcripts, enriched with conversation text, tool calls, and model names read from Cursor's database | Spilled tool output, terminal captures, agent scratchpad notes |
| GitHub Copilot | Copilot CLI session event logs; Copilot Chat transcripts and VS Code's chat session store, which holds tool results and token counts, including chats opened with no folder and sessions moved between workspaces | CLI session plans, checkpoints, research reports, session files and rewind snapshots; personal `copilot-instructions.md` and `instructions/*.instructions.md`; VS Code chat editing checkpoints and file baselines |
| Pi (experimental) | Native JSONL session trees, messages, usage, branches and tool results | None |
| OpenCode (experimental) | Per-session JSONL snapshots of session, message and part records from `opencode.db` | None |
| Hermes (experimental) | Per-session JSONL snapshots of messages and model usage from `state.db`, including named profiles | None |

Pi uses `$PI_CODING_AGENT_SESSION_DIR`, `$PI_CODING_AGENT_DIR/sessions`, or
`~/.pi/agent/sessions`. OpenCode uses `$XDG_DATA_HOME/opencode` or
`~/.local/share/opencode`. Hermes uses `$HERMES_HOME` or `~/.hermes`, including
`profiles/*/state.db`. As with other sources, the first existing root wins; set the corresponding
environment variable in the shipper process or configure `sources[].roots` through
the control plane for nonstandard locations. For example:

```yaml
sources:
  - id: opencode-sessions
    roots: ["/srv/agents/opencode"]
```

OpenCode and Hermes export selected session records as scrubbed JSONL, including
committed WAL changes. Database files and credentials are not uploaded. Each
snapshot is capped at 64 MiB by default; unchanged snapshots are not uploaded again.

A repository marked as not tracked is matched through the folder the session ran in. For Copilot Chat this is the folder VS Code opened; in a multi-root workspace a mark on any of its folders applies. Chats from a remote VS Code window (SSH, WSL, dev container) name a folder on another machine, so they ship without a repository and a mark there does not apply.

One more record is produced by the shipper itself:

- **Account and usage history.** Independent Claude Code, Codex, and Cursor collectors upload
  account metadata and provider usage JSON in UTC buckets matching the collection interval (default
  15 minutes). Unknown fields are preserved; these records skip scrubbing and ship encrypted.

All other files above pass through the scrub stage before encryption. The control plane receives no
file content. It receives the install id, hostname, platform, and agent version in each heartbeat.

Never uploaded as files: credential stores such as Claude Code's `.credentials.json` and
`~/.claude.json`, Codex's `auth.json`, Cursor's `state.vscdb`, the Copilot CLI's `config.json` (which also holds its
settings), MCP configuration and secrets, and IDE lock files, and VS Code's global `state.vscdb`. A compiled deny list blocks
these paths even when a configured glob would match them. Collectors and enrichers read only
the data they need from these stores:

- Account collectors read account metadata and use credentials only to authenticate provider requests.
- The Cursor enricher reads conversation records from `state.vscdb` and writes the conversation
  text, tool calls, and model names it finds into the transcript record, because Cursor's own
  transcript files lack them. The `cursorAuth/*` keys and the encryption-key fields are stripped
  by a compiled rule that no configuration layer can disable.

Everything derived this way passes through the scrub stage like any other file.

`quesma-shipper tracking` shows what is collected on this machine, per agent and repository.
`quesma-shipper preview` shows what the next run would send, after scrubbing, without sending it.

## Usage

```
quesma-shipper                Status: is it on, what is collected, what is waiting to be sent
quesma-shipper login <token>  Join your organisation with the token from your admin
quesma-shipper tracking       What is collected, per agent and repository
quesma-shipper pause [dur]    Stop collecting for 15m, 1h, 6h, 12h, 24h, or until tomorrow 9:00
quesma-shipper resume         Start collecting again
quesma-shipper doctor         Check collecting and sending, explain anything wrong
quesma-shipper update         Install the newest release
quesma-shipper uninstall      Remove the service and program, keep local state
quesma-shipper licenses       Print the license and the third-party notices
```

Debug commands. These are not shown in `--help`:

```
quesma-shipper run [--once|--drain] [-q]   Run the scheduler loop in the foreground, log to stderr
quesma-shipper preview                      Show what would be sent, without sending or recording anything
quesma-shipper config [--with-provenance]   The effective configuration and where each value came from
quesma-shipper log                          Recent audit entries: what was decided about each file
quesma-shipper state reset|prune            Inspect and repair the local record of what was sent
quesma-shipper service uninstall|restart    Manage the background service entry
quesma-shipper local-dev                    Set up without a control plane
```

To look at the pipeline without any control plane, run `quesma-shipper local-dev`, then
`quesma-shipper preview`: collection and scrubbing work, and upload is disabled.

## Configuration

Configuration is resolved from four layers, in this order: compiled defaults, the source catalog,
the user file, the document served by the control plane. `quesma-shipper config --with-provenance`
prints each effective value and the layer that set it.

The served document has limited authority. A rulebook in the
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) module states, field by field,
what the control plane can change. For example, it can add scrub rule packs. It cannot remove them.
It cannot turn off scrub or encryption. Contract tests here and in the control plane enforce the
rulebook.

File locations. `XDG_CONFIG_HOME` and `XDG_STATE_HOME` are honoured.

| Path | Contents |
|---|---|
| `~/.config/trajectory-shipper/config.yaml` | Optional user layer |
| `~/.local/state/trajectory-shipper/` | Identity, upload record, audit log, run logs |

`trajectory-shipper` remains the wire identifier and the on-disk configuration and state namespace,
separate from the user-facing Quesma Shipper package and command.

**Upload targets.** The shipper refuses an upload ticket that is not HTTPS, so a store such as a
local MinIO needs an `upload_targets` entry in the user file, as in
[On one machine](../fleet-manager/README.md#on-one-machine). The pin lives there and only there: a presigned
ticket authorises itself, so a served document that could write the pin could also loosen it.

Defaults: a 15-minute schedule, a maximum of 512 files per run, a 5-minute drain deadline.

Environment variables:

| Variable | Effect |
|---|---|
| `SHIPPER_AUTH_KEY` | Supplies the login token without a prompt |
| `SHIPPER_NO_SELFUPDATE` | Any value disables the self-update check |

## Security model

- Trajectory files contain prompts, source code, shell output, and often credentials. Treat local
  state, logs, and encrypted bundles as confidential.
- Scrub and encrypt are mandatory pipeline stages in every build. No configuration layer can
  remove them.
- The control plane distributes configuration and authorises uploads. It does not proxy, receive,
  or store trajectory content.
- Uploads use per-object presigned URLs. The client holds no long-lived storage credential.
- One install cannot overwrite another install's objects. The object-key grammar and the per-upload
  authorisation enforce this.
- Updates are verified against an offline-signed TUF root that is embedded in the binary.

Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Building it

You need Go 1.27 or newer. `make build` at the repository root puts the binary in
`bin/quesma-shipper`; `make install` puts it in GOBIN. Development builds report their commit and
never self-update. Installers, cross-compiling and releases are in
[CONTRIBUTING.md](../CONTRIBUTING.md#build-installers-and-releases).
