# Contributing

This document covers setup, the commit gate, design rules, code style, and pull requests. All
participation is subject to the [code of conduct](CODE_OF_CONDUCT.md).

## Setup

- Install Go 1.27 or newer. No other tool is required. `make doctor` lists the optional ones.
- Clone the repository. Run `make build` and `make test`.
- The Go module root is `src/`. Run `go` commands from there, or use the Makefile from the
  repository root.
- The control plane, [Fleet Manager](fleet-manager/), is a second component with its own Go module,
  Makefile and release line under `fleet-manager/`. It needs Go 1.27 and Node 22 or newer, for the
  administration UI's tests. Run its targets with `make -C fleet-manager <target>`; `make -C
  fleet-manager help` lists them. Nothing in one component builds or tests the other.
- The wire contract, [shipper-protocol/](shipper-protocol/), is a third Go module that both
  components build against through a `replace` directive. Its gate is
  `make -C shipper-protocol check`.

## Before you open a pull request

```sh
make check
```

This is the commit gate. CI runs the same command. It runs gofmt and go vet, with a Windows
type-check. It then runs VERSION validation, dead-code detection, and the dependency license check.
Last, it runs the unit suite under the race detector. `make test` is
the faster local loop.

For a change under `fleet-manager/`, the gate is `make -C fleet-manager check`: gofmt, go vet,
VERSION, its own dependency license check, the Go suite, and the administration UI's tests. CI runs
both gates on every pull request. [fleet-manager/AGENTS.md](fleet-manager/AGENTS.md) lists what
differs there, such as the Terraform grants its tests pin.

Open the pull request as a draft. A maintainer marks it ready for review. Put small fixes on the
same branch. Put a larger follow-up in a stacked pull request.

In the description, state what changed and why. Give a concrete example of the behaviour before
and after. Include a binary-size or performance diff when the change can affect either.

## Build, installers and releases

```sh
make build       # bin/quesma-shipper
make test        # unit suite and local end-to-end tests
make race        # the same under the race detector
make check       # the commit gate: fmt, vet, version, dead code, licenses, race
make perf-smoke  # PR-sized performance gates, needs Docker
make perf        # full performance suite, needs Docker
make help        # every target
```

CI runs `make check` and `make perf-smoke`. `make perf` runs the full performance tier against a
local MinIO and Toxiproxy. Both perf targets need Docker and are skipped without it. Tests that run
the shipper against a live Fleet Manager are not part of this repository yet.

To test the Linux installer and the background service with a local build, `make build` and then
`sh src/packaging/linux/install.sh --from bin/quesma-shipper`; it installs into `~/.local/bin` for
your user, so do not run it as root. `make install` puts the binary in GOBIN instead, with no
service. Neither self-updates: development builds report their commit and never fetch a release.

Installers come from the same tree. `make macos-pkg RELEASE_VERSION=<version>` builds the macOS
package, on macOS only, and `make dist RELEASE_VERSION=<version>` cross-compiles both Windows
binaries. On Windows with Inno Setup installed:

```powershell
src/packaging/windows/build-setup.ps1 -ReleaseVersion <version> -Architecture amd64 `
  -BinaryPath <binary> -SupervisorPath <supervisor-binary> -OutputDir bin/dist
```

`VERSION` contains the reviewed `MAJOR.MINOR.PATCH` release line. Change it only to start a new
line. `make version-check` validates it. Release builds are stamped
`<VERSION>-<commit count>.<short sha>` by `scripts/release-version.sh`; `make release-version`
prints the stamp.

A push to `main` that touches the shipper builds six platform binaries and the macOS package, signs
TUF metadata, and publishes to `https://updates.quesma.dev`; the public repository and the stable
download endpoints are described in [RELEASE_DOWNLOADS.md](RELEASE_DOWNLOADS.md). It then creates
a [GitHub release](https://github.com/QuesmaOrg/quesma-shipper/releases) linking the stable
downloads. A maintainer approves each publication. Each release also carries a `quesma-shipper.rb`
cask pinned to the published macOS binaries, which the
[Homebrew tap](https://github.com/QuesmaOrg/homebrew-tap) imports hourly or on a manual workflow
run; see [Homebrew packaging](src/packaging/homebrew/README.md). A push to `main` that touches
`fleet-manager/` publishes `quesma/fleet-manager:<sha>` and `:latest` to Docker Hub, which the
deployment templates pull.

The dependency notices ship inside the binaries: `quesma-shipper licenses` prints them, and they
are kept in `src/internal/legal/third_party/` with an inventory in `licenses.csv` that covers
Linux, macOS and Windows builds. `make licenses` regenerates them after a dependency change, and
`make -C fleet-manager licenses` does the same for `fleet-manager/third_party/`.

Repository layout:

```
src/           Go module root
  cmd/quesma-shipper/   main
  app/           composes a run
  internal/      pipeline stages, config, control-plane client, platform floor
  packaging/     install, service, and update mechanics per OS
  internal/legal embedded LICENSE, NOTICE, and third-party license texts
  e2e/           hermetic end-to-end and golden tests
Makefile       repository-level build and test entry points
fleet-manager/ the control plane: its own Go module, Makefile, VERSION, NOTICE and third_party/
  src/           the service and its embedded administration UI
  terraform/     deployment templates for AWS and Google Cloud
  deploy.sh      applies a template into a cloud account without a clone
```

## Design rules

[CONSTITUTION.md](CONSTITUTION.md) is the design authority. A change that conflicts with an
article is not merged. A change to the constitution is its own pull request. Argue the trade-off
in the description.

[ARCHITECTURE.md](ARCHITECTURE.md) states which package can import which. Follow the table. If a
change needs a new edge, say so in the description. The table is the shipper's; Fleet Manager is one
package, and [fleet-manager/ARCHITECTURE.md](fleet-manager/ARCHITECTURE.md) holds its invariants.

Reviewers check these invariants on every shipper change:

- Scrub fails closed. A scrub error means the file is not uploaded.
- No configuration layer can remove scrub or encrypt.
- No cloud SDK is imported. Upload is `net/http` only.
- Only the packages that own a durable artifact write to disk directly. All other writes go
  through `safeio`. The shipper never writes inside an agent's store.

## Golden tests and compatibility

Golden tests pin output byte for byte. They are `src/e2e/golden_test.go`, the conformance vectors
under `src/conformance/`, and the wire fixtures under `shipper-protocol/fixtures/` that the
contract tests import. Fleet Manager's contract
is pinned the same way, by `fleet-manager/src/protocol_test.go` and the same wire fixtures, and
what its deployment templates may grant by `fleet-manager/src/terraform_test.go`. A golden diff is
a claim that the output must change. Read the diff. Explain it in the pull request. Do not run with
`-update` until a maintainer agrees.

File formats, the wire protocol, configuration keys, and the object-key grammar are compatibility
surfaces. A change to one of them needs a test that shows old inputs still work, and a note in the
pull request. A protocol change is made under [shipper-protocol/](shipper-protocol/), in the same
pull request as the shipper and Fleet Manager changes that need it: both build against that
directory, so their contract tests run on the same tree. Record it in the protocol's `CHANGELOG.md`;
a release is a tag `shipper-protocol/vX.Y.Z`. [shipper-protocol/AGENTS.md](shipper-protocol/AGENTS.md)
lists what a released wire version may never change.

Do not widen a performance budget. Do not make a test less strict. If you must, say so in the pull
request.

## Code style

- Comment the intent and the non-obvious corner cases. Do not describe what the code does.
- Use about three lines of top-level comment per file. Use one line elsewhere.
- Keep dependencies few. Argue each new one in the pull request. `make check` rejects licenses
  outside Apache-2.0, MIT, BSD, ISC, and Unlicense, and so does `make -C fleet-manager check`. A new
  Fleet Manager dependency also means running `make -C fleet-manager licenses` and committing the
  regenerated `fleet-manager/third_party/`.
- After a large change, simplify before you ask for review.

## Test data

Do not commit real trajectories, prompts, transcripts, agent databases, logs, encrypted bundles,
credentials, or private project data. Use small synthetic fixtures. Use `example.com` addresses,
placeholder usernames, and invented project names. `.gitignore` blocks the common file types as a
last line of defence.

If you need real data to reproduce a bug, minimise and redact it locally. Then share it through a
private channel as described in [SECURITY.md](SECURITY.md).

## License

Contributions are accepted under the Apache License 2.0, the license of the project. When you
submit a pull request, you confirm that you have the right to license your contribution under
those terms.
