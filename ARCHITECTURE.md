# Architecture

This document describes the module's shape and the reasoning behind it. It is intent,
written for humans; the placement rules below are conventions to follow in review, not
checks a tool runs.

## Shape

**The tree is the architecture.** Packages are named for pipeline responsibilities, and the
shipper's one-sentence description maps onto the directory listing: everything the product
does is one pipeline (read from sources, transform, upload) pumped by one engine, invoked by
three triggers that differ only in *when*.

```
quesma-shipper/
  src/                    Go module root
    cmd/quesma-shipper/   main
    app/                  the facade: resolve config into a run, flush it — the one public package
    packaging/            install, update, service lifecycle; thin current-OS dispatch
      common/             TUF verification, re-exec, shared service types and run markers
      macos/              app/pkg assets, launchd, app-bundle update and removal
      linux/              systemd user service
    internal/
      cli/                cobra verbs and rendering — formats, never decides
      config/             policy: the layer merge, the authority rulebook, the served document
      identity/           who this install is: the Unit (install id, age keys, name_key), mint-once, 0600
      engine/             the pump: discover → detect change → read → transform → upload → commit.
                          Owns the upload port, the fingerprint state and the heartbeat.
      sources/            WHAT to read and HOW: catalog resolution, gather, deny, pathexp
        sqliteread/       live agent databases; the sqlite dependency stops here
      transforms/         what changes between read and write: scrub → enrich → seal
        packs/            the compiled redaction rule packs (data, importable by config)
        cursorjoin/       the Cursor enricher
      upload/             the presigned PUT: origin allowlist, exact-key validation, net/http
                          and nothing else. No cloud SDK exists in this module.
      controlplane/       everything that speaks to the control plane, regardless of verb:
                          the enroll call, the config fetch, upload authorization, the
                          enrollment record
      platform/           the inert floor, one stated invariant each: atomic writes (safeio),
                          the kill switch (pause), memstat, buildinfo
      legal/              the embedded LICENSE, NOTICE and third-party license texts
      formats/            the shared vocabulary: local-contract schemas, the model types,
                          the object-key naming grammar (+ its conformance vectors)
        catalogdata/      the embedded source-catalog YAML
```

Paths in this document are relative to the repository root. The Go module begins at `src/`,
so Go import paths omit that filesystem prefix.

Import direction follows the table below: **every internal edge is declared, package by
package.** Go's compiler bans cycles, not wrong-direction edges: `transforms → engine`
would compile, so the table is the reference for what direction is intended.

| Package | May import |
| --- | --- |
| `src/app` | `src/internal/config`, `src/internal/controlplane`, `src/internal/engine`, `src/internal/formats`, `src/internal/identity`, `src/internal/platform`, `src/internal/platform/auditlog`, `src/internal/sources`, `src/internal/transforms`, `src/internal/transforms/accountprobe`, `src/internal/transforms/cursorjoin`, `src/internal/upload`, `src/packaging` |
| `src/cmd/quesma-shipper` | `src/app`, `src/internal/cli`, `src/internal/platform` |
| `src/e2e` | — |
| `src/internal/cli` | `src/app`, `src/internal/config`, `src/internal/controlplane`, `src/internal/engine`, `src/internal/formats`, `src/internal/identity`, `src/internal/legal`, `src/internal/platform`, `src/internal/platform/auditlog`, `src/internal/platform/crashjournal`, `src/internal/sources`, `src/packaging` |
| `src/internal/config` | `src/internal/platform`, `src/internal/sources`, `src/internal/transforms`, `src/internal/transforms/packs` |
| `src/internal/controlplane` | `src/internal/config`, `src/internal/formats`, `src/internal/platform` |
| `src/internal/engine` | `src/internal/formats`, `src/internal/identity`, `src/internal/platform`, `src/internal/platform/auditlog`, `src/internal/sources`, `src/internal/transforms` |
| `src/internal/formats` | — |
| `src/internal/legal` | — |
| `src/internal/formats/catalogdata` | — |
| `src/internal/identity` | `src/internal/formats`, `src/internal/platform` |
| `src/internal/platform` | — |
| `src/internal/platform/auditlog` | `src/internal/formats`, `src/internal/platform` |
| `src/internal/platform/crashjournal` | `src/internal/platform` |
| `src/internal/sources` | `src/internal/formats`, `src/internal/formats/catalogdata`, `src/internal/platform` |
| `src/internal/sources/sqliteread` | — |
| `src/internal/transforms` | `src/internal/formats`, `src/internal/transforms/packs` |
| `src/internal/transforms/accountprobe` | `src/internal/sources/sqliteread`, `src/internal/transforms` |
| `src/internal/transforms/cursorjoin` | `src/internal/sources/sqliteread`, `src/internal/transforms` |
| `src/internal/transforms/packs` | — |
| `src/internal/upload` | — |
| `src/packaging` | `src/packaging/common`, and the current OS package (`src/packaging/macos` or `src/packaging/linux`) |
| `src/packaging/common` | `src/internal/platform` |
| `src/packaging/macos` | `src/internal/platform`, `src/packaging/common` |
| `src/packaging/linux` | `src/internal/platform`, `src/packaging/common` |

Banned by name, with the temptation each forestalls:

- `src/internal/sources → src/internal/engine` — a stage feeds the pump; it does not reach back into it
- `src/internal/transforms → src/internal/engine` — a stage feeds the pump; it does not reach back into it
- `src/internal/platform → src/internal/*` — the floor is inert: network-capable chokepoints are sub-packages, not dependencies

Two edges deserve prose because they look like mistakes and are the design:

- **`src/app` is the only package importing both `src/internal/controlplane` and
  `src/internal/upload`.** The engine
  declares the port (`UploadPort`, `PreparedObject`, `PutResult`) and is handed an
  implementation; the ticket, the URL and every wire type stop in `src/app/`, which is why the
  loop cannot learn that a control plane exists.
- **`src/internal/controlplane → src/internal/config`**: the served document is config's format to parse; the
  alternative is a second implementation of the same document.

## The invariants that outrank the tree

These are checked by tests or held in review, because no directory layout can express them:

- **Fail-closed scrub**: a scrub error means the file does not upload. Scrub and seal are
  mandatory pipeline steps in every build; no config layer can remove them.
- **Write-path lint** (`src/internal/platform/writepath_lint_test.go`): only the packages (and,
  inside merged packages, the specific *files*) that own a durable artifact may call the
  os-level write functions; everything else routes through safeio. The shipper never writes
  inside an agent's store.
- **Third-party confinement**: an SDK stops in the package that owns it — sqlite never
  past sqliteread. No AWS, Azure or Google client is linked into the shipped binary
  (the `-tags perf` harness in `src/perf` is the sole importer of the S3 SDK, as a test
  peer): the only route to object storage is a presigned ticket spent over `net/http`,
  and a credential this binary never holds cannot be misdirected. `go list -deps
  ./cmd/quesma-shipper` is the check that nothing from the harness reaches the binary.
- **Surface caps**: platform and formats are importable from everywhere, so their
  exported surfaces stay small; growth there is a decision, not an accident.

## Placement heuristic

Ask **which verb makes this code run**:

- during `sync`/`run` → it is a pipeline concern: a stage (`sources`, `transforms`,
  `upload`), the pump (`engine`), policy (`config`), or the wire client (`controlplane`).
- installation, update, service registration, or removal → `src/packaging/`; reusable policy goes
  in `src/packaging/common/`, OS mechanisms beside their installer assets.
- in every build regardless of verb, with one stated invariant → `src/internal/platform/`.
- it describes data at rest or on the wire → `src/internal/formats/`.
- it composes a run → `src/app/`. It renders → `src/internal/cli/`.

One deliberate exception: `src/internal/controlplane/` is organized by *counterparty*, not by verb — the
enroll call (setup-time) lives beside the fetch and the upload authorization (sync-time)
because they share one wire contract, pinned by `src/internal/controlplane/wire_contract_test.go`
against the schemas and fixtures embedded in the shipper-protocol module.

## History

The first generation of this file described the same invariants as eight pattern-role layers
(contract → platform → domain → ports → core → adapters → app → cli) over 37 packages,
enforced by a layer map and an exception ratchet. The 2026-08 pipeline-tree migration merged
the packages into the tree above and replaced the layer machinery with the per-edge
allowlist; the invariants themselves — zero-network, fail-closed scrub, write-path
discipline, dependency confinement — moved unchanged. The design discussion and migration
plan were recorded separately in the old repository and were not copied here.
