## Design

Design decisions are governed by [CONSTITUTION.md](CONSTITUTION.md).

## Workflow

- Open a draft PR when you are done. Only humans mark a branch ready to review. Fixes go to the same branch, but bigger follow-ups should use a stacked PR. Ask if unsure.

## Before opening a PR

Consider running `make race` and `make perf` (needs Docker) when a change has a risk of regressing performance or binary size. If appropriate, include the binary-size or performance diff. Please offer to run adversarial code review. Please give easy to understand examples in PR description.

## Style

1. Comment only the high level intent or unobvious corner cases, do not describe what logic does.

2. Typically 3 lines of top-level comments per file is enough. Do more only if you were told to do that.

3. One line of comment elsewhere in file is enough. Be cautious with more.

4. Please offer to run simplification after finishing big change (e.g. `/simplify`).

5. Be cautious with file format changes, protocol changes or configuration changes. They could break backward compatibility. Test that and verify assumptions with a human. Golden tests (`src/e2e/golden_test.go`, `src/conformance/`, and the wire fixtures imported from the shipper-protocol module) pin output byte for byte; a golden diff is a claim the output should change, so read the diff and ask for confirmation before running `-update`.

6. Never widen perf budgets or make tests less strict without explicit confirmation.
