# Quesma Shipper

Quesma Shipper collects the session files and related artifacts that AI coding agents write on a
developer machine. See [What is collected](#what-is-collected). It removes secrets and personal
data from each file, encrypts the file with
[age](https://age-encryption.org/), and uploads the ciphertext to your organisation's object
storage. It runs in the background. It is written in Go and ships as one static binary. A central
control plane manages every install.

Supported agents: Claude Code, Codex, Cursor.

Supported platforms: macOS 13 or newer (app bundle, launchd service), Linux (systemd user
service), Windows (binary only, no service).

## Status

The project is pre-1.0. The wire protocol, the configuration format, and the object naming are
versioned and pinned by tests. They can still change between minor releases.

<!-- TODO(control-plane-oss): link the control-plane repository here.  https://github.com/QuesmaOrg/quesma-shipper/issues/1 -->
The shipper needs a control plane to send data. The control plane is not part of this repository
and is not published yet. Without a control plane the shipper runs in local development mode.
Collection, scrubbing, and preview work. Upload is disabled.

The public update channel is a test facility, not a supported installation channel. See
[Versions and releases](#versions-and-releases).

`trajectory-shipper` is the product, service, and wire identifier. It appears in file paths,
service names, and the User-Agent header. It is kept for compatibility.

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

The design rules are in [CONSTITUTION.md](CONSTITUTION.md). The code layout is in
[ARCHITECTURE.md](ARCHITECTURE.md). The wire contract is the public
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) module. This repository imports its schemas and
fixtures. Protocol changes are reviewed and released there.

## What is collected

The shipper collects more than chat transcripts. Per agent, from the agent's own directories:

| Agent | Trajectories | Context artifacts |
|---|---|---|
| Claude Code | Session and subagent transcripts, spilled tool results | Per-project memory files, plans, todos, file-history checkpoint metadata, the user-level `CLAUDE.md`, `settings.json` |
| Codex | Sessions, archived and compressed sessions, the session index | None |
| Cursor | Agent transcripts, enriched with conversation text, tool calls, and model names read from Cursor's database | Spilled tool output, terminal captures, agent scratchpad notes |

Two more records are produced by the shipper itself:

- **Project map.** For each project directory an agent used: the directory name, the working
  directory, and the git remote as host and path. Credentials embedded in a remote URL are removed
  before the value is written anywhere.
- **Account metadata.** From each agent's account store: email, plan, rate-limit tier, auth mode,
  billing type, organisation name and role, seat tier, team, and subscription end date. Tokens
  cannot appear in this record. The output is a fixed struct, so unknown fields are dropped.

Every file above passes through the scrub stage before encryption. The control plane receives no
file content. It receives the install id, hostname, platform, and agent version in each heartbeat.

Never uploaded as files: credential stores such as Claude Code's `.credentials.json` and
`~/.claude.json`, Codex's `auth.json`, and Cursor's `state.vscdb`. A compiled deny list blocks
these paths even when a configured glob would match them. Two enrichers read from them and ship
only derived fields:

- The account probe reads the account fields listed above into a fixed struct. A token cannot
  appear in it.
- The Cursor enricher reads conversation records from `state.vscdb` and writes the conversation
  text, tool calls, and model names it finds into the transcript record, because Cursor's own
  transcript files lack them. The `cursorAuth/*` keys and the encryption-key fields are stripped
  by a compiled rule that no configuration layer can disable.

Everything derived this way passes through the scrub stage like any other file.

`shipper tracking` shows what is collected on this machine, per agent and repository.
`shipper preview` shows what the next run would send, after scrubbing, without sending it.

## Install

### From a release

**macOS**

Install the signed `Shipper-macos-universal.pkg`. It installs `Shipper.app` for the current user and
registers a launchd agent. It does not need administrator rights.

**Linux**

```sh
sh src/packaging/linux/install.sh
```

The script downloads a hash-pinned bootstrap binary and installs a systemd user service. Run it
again to upgrade. Existing enrollment is kept. Use `--no-service` to skip the service.

**Windows**

Download `shipper-windows-<arch>.exe` from a release, rename it to `shipper.exe`, and put it in a
directory on `PATH`. Run `shipper run` from a scheduler of your choice. There is no service
integration yet.

### From source

You need Go 1.27 or newer. No other tool is required. `make doctor` lists the optional ones.

```sh
make build                                     # bin/shipper
make install                                   # into GOBIN, or GOPATH/bin
```

To test the Linux installer and the background service with a local build:

```sh
make build
sh src/packaging/linux/install.sh --from bin/shipper
```

To build the macOS package: `make macos-pkg RELEASE_VERSION=<version>`. This works on macOS only.

### Enroll

Every new shipper must be enrolled with the control plane selected by your administrator. The
command is the same on every platform:

```sh
shipper login <enrollment-token> --server https://cp.example.com
```

To run the pipeline without a control plane:

```sh
bin/shipper local-dev      # create a local identity and state directory
bin/shipper preview        # show what would be collected and how it would be scrubbed
```

## Usage

```
shipper                Status: is it on, what is collected, what is waiting to be sent
shipper login <token>  Join your organisation with the token from your admin
shipper tracking       What is collected, per agent and repository
shipper pause [dur]    Stop collecting for 15m, 1h, 6h, 12h, 24h, or until tomorrow 9:00
shipper resume         Start collecting again
shipper doctor         Check collecting and sending, explain anything wrong
shipper update         Install the newest release
shipper uninstall      Remove the service and program, keep local state
shipper licenses       Print the license and the third-party notices
```

Debug commands. These are not shown in `--help`:

```
shipper run [--once|--drain] [-q]   Run the scheduler loop in the foreground, log to stderr
shipper preview                      Show what would be sent, without sending or recording anything
shipper config [--with-provenance]   The effective configuration and where each value came from
shipper log                          Recent audit entries: what was decided about each file
shipper state reset|prune            Inspect and repair the local record of what was sent
shipper service uninstall|restart    Manage the background service entry
shipper local-dev                    Set up without a control plane
```

## Configuration

Configuration is resolved from four layers, in this order: compiled defaults, the source catalog,
the user file, the document served by the control plane. `shipper config --with-provenance` prints
each effective value and the layer that set it.

The served document has limited authority. A rulebook in the
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) module states, field by field, what the control plane
can change. For example, it can add scrub rule packs. It cannot remove them. It cannot turn off
scrub or encryption. Contract tests here and in the control plane enforce the rulebook.

File locations. `XDG_CONFIG_HOME` and `XDG_STATE_HOME` are honoured.

| Path | Contents |
|---|---|
| `~/.config/trajectory-shipper/config.yaml` | Optional user layer |
| `~/.local/state/trajectory-shipper/` | Identity, upload record, audit log, run logs |

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

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Repository layout

```
src/           Go module root
  cmd/shipper/   main
  app/           composes a run
  internal/      pipeline stages, config, control-plane client, platform floor
  packaging/     install, service, and update mechanics per OS
  internal/legal embedded LICENSE, NOTICE, and third-party license texts
  e2e/           hermetic end-to-end and golden tests
Makefile       repository-level build and test entry points
```

## Build and test

```sh
make build       # bin/shipper
make test        # unit suite and local end-to-end tests
make race        # the same under the race detector
make check       # the commit gate: fmt, vet, version, dead code, licenses, race
make perf-smoke  # PR-sized performance gates, needs Docker
make perf        # full performance suite, needs Docker
make help        # every target
```

<!-- TODO(control-plane-oss): link the control-plane repository here. https://github.com/QuesmaOrg/quesma-shipper/issues/1 -->
CI runs `make check` and `make perf-smoke`. `make perf` runs the full performance tier against a
local MinIO and Toxiproxy. Both perf targets need Docker and are skipped without it. Cross-service
tests that need the control plane are not part of this repository.

## Versions and releases

`VERSION` contains the reviewed `MAJOR.MINOR.PATCH` release line. Change it only to start a new
line. `make version-check` validates it. Release builds are stamped
`<VERSION>-<commit count>.<short sha>` by `scripts/release-version.sh`. `make release-version`
prints the stamp. Development builds are not stamped. They report their commit and do not
self-update.

A push to `main` that touches the shipper builds six platform binaries and the macOS package, signs
TUF metadata, and publishes to `https://updates.quesma.dev`. The first release through this
workflow was published on 2026-09-04. A maintainer approves each publication.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Do not attach
real trajectories, agent databases, logs, or credentials to issues or pull requests.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). `shipper licenses` prints the
license and every bundled dependency's license text; the same texts ship inside the macOS app
bundle and are kept in `src/internal/legal/third_party/` with an inventory in `licenses.csv` that
covers Linux, macOS, and Windows builds. `make licenses` regenerates them after a dependency change.
`make check` fails when they are stale or when a dependency is not under a permissive license.

Quesma, Quesma Shipper, and the Quesma logo are trademarks of Quesma Inc. The license does not
grant trademark rights. If you distribute a modified build, read [TRADEMARKS.md](TRADEMARKS.md)
before you name it.
