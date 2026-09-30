# The shipper

What runs on each developer machine: how it works, what it collects, and how to use and configure
it. Installing and enrolling it is in the repository [README](../README.md#install-the-shipper).

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

One more record is produced by the shipper itself:

- **Account and usage history.** Independent Claude Code, Codex, and Cursor collectors upload
  account metadata and provider usage JSON in UTC buckets matching the collection interval (default
  15 minutes). Unknown fields are preserved; these records skip scrubbing and ship encrypted.

All other files above pass through the scrub stage before encryption. The control plane receives no
file content. It receives the install id, hostname, platform, and agent version in each heartbeat.

Never uploaded as files: credential stores such as Claude Code's `.credentials.json` and
`~/.claude.json`, Codex's `auth.json`, and Cursor's `state.vscdb`. A compiled deny list blocks
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
[On one machine](../README.md#on-one-machine). The pin lives there and only there: a presigned
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
