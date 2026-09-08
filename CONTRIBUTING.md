# Contributing

This document covers setup, the commit gate, design rules, code style, and pull requests. All
participation is subject to the [code of conduct](CODE_OF_CONDUCT.md).

## Setup

- Install Go 1.27 or newer. No other tool is required. `make doctor` lists the optional ones.
- Clone the repository. Run `make build` and `make test`.
- The Go module root is `src/`. Run `go` commands from there, or use the Makefile from the
  repository root.

## Before you open a pull request

```sh
make check
```

This is the commit gate. CI runs the same command. It runs gofmt and go vet, with a Windows
type-check. It then runs VERSION validation, dead-code detection, and the dependency license check.
Last, it runs the unit suite under the race detector. `make test` is
the faster local loop.

Open the pull request as a draft. A maintainer marks it ready for review. Put small fixes on the
same branch. Put a larger follow-up in a stacked pull request.

In the description, state what changed and why. Give a concrete example of the behaviour before
and after. Include a binary-size or performance diff when the change can affect either.

## Design rules

[CONSTITUTION.md](CONSTITUTION.md) is the design authority. A change that conflicts with an
article is not merged. A change to the constitution is its own pull request. Argue the trade-off
in the description.

[ARCHITECTURE.md](ARCHITECTURE.md) states which package can import which. Follow the table. If a
change needs a new edge, say so in the description.

Reviewers check these invariants on every change:

- Scrub fails closed. A scrub error means the file is not uploaded.
- No configuration layer can remove scrub or encrypt.
- No cloud SDK is imported. Upload is `net/http` only.
- Only the packages that own a durable artifact write to disk directly. All other writes go
  through `safeio`. The shipper never writes inside an agent's store.

## Golden tests and compatibility

Golden tests pin output byte for byte. They are `src/e2e/golden_test.go`, the conformance vectors
under `src/conformance/`, and the wire fixtures that the contract tests import from the
[shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol) module. A golden diff is a claim
that the output must change. Read the diff. Explain it in the pull request. Do not run with
`-update` until a maintainer agrees.

File formats, the wire protocol, configuration keys, and the object-key grammar are compatibility
surfaces. A change to one of them needs a test that shows old inputs still work, and a note in the
pull request. A protocol change is made in the [shipper-protocol](https://github.com/QuesmaOrg/shipper-protocol)
repository and released as a module version. A shipper change that needs it bumps the dependency
after that release.

Do not widen a performance budget. Do not make a test less strict. If you must, say so in the pull
request.

## Code style

- Comment the intent and the non-obvious corner cases. Do not describe what the code does.
- Use about three lines of top-level comment per file. Use one line elsewhere.
- Keep dependencies few. Argue each new one in the pull request. `make check` rejects licenses
  outside Apache-2.0, MIT, BSD, ISC, and Unlicense.
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
