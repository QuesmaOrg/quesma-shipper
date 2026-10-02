## Design

Design decisions are governed by [CONSTITUTION.md](CONSTITUTION.md).

The control plane in `fleet-manager/` is its own component. For a change there, also follow
[fleet-manager/AGENTS.md](fleet-manager/AGENTS.md), which lists what differs, and run
`make -C fleet-manager check`.

## Workflow

- Open a draft PR when you are done. Only humans mark a branch ready to review. Fixes go to the same branch, but bigger follow-ups should use a stacked PR. Ask if unsure.

## Code Review Rules

For PR reviews, first check correctness, regressions, security and compatibility. Then use the
component review skills as an additional pass over the changed behavior:

- Shipper code, packaging, scripts, release workflows and documentation: read
  [.claude/skills/review-shipper/SKILL.md](.claude/skills/review-shipper/SKILL.md).
- Fleet Manager code, Terraform, deployment, UI, workflows and documentation: read
  [.claude/skills/review-fleet-manager/SKILL.md](.claude/skills/review-fleet-manager/SKILL.md).

Read the selected skill's `checklist.md` and `gotchas.md` too, and apply only the relevant checks.
For changes affecting both components, use both and check their shared contract; do not choose
by file count. Verify each claim against the reviewed base and head, since the skills describe
behavior that may differ at those revisions. Read existing review threads only after your own
passes, then reconcile duplicates. Historical PR citations are background, not an answer key.

For automated Codex PR reviews, the review runner's supplied target, tools, output format and
severity policy take precedence over the skills' manual target-resolution and reporting steps.
Return findings through the runner; do not create PRs, approve, request changes, or post separate
comments with `gh`. Report concrete problems introduced or exposed by the diff, not checklist
completion or missing approval alone. If a referenced skill or required evidence is unavailable,
continue the general review and state the limitation where the runner's format permits it.

## Before opening a PR

Consider running `make race` and `make perf` (needs Docker) when a change has a risk of regressing performance or binary size. If appropriate, include the binary-size or performance diff. Please offer to run adversarial code review. Please give easy to understand examples in PR description.

## Style

1. Comment only the high level intent or unobvious corner cases, do not describe what logic does.

2. Typically 3 lines of top-level comments per file is enough. Do more only if you were told to do that.

3. One line of comment elsewhere in file is enough. Be cautious with more.

4. Please offer to run simplification after finishing big change (e.g. `/simplify`).

5. Be cautious with file format changes, protocol changes or configuration changes. They could break backward compatibility. Test that and verify assumptions with a human. Golden tests (`src/e2e/golden_test.go`, `src/conformance/`, and the wire fixtures imported from the shipper-protocol module) pin output byte for byte; a golden diff is a claim the output should change, so read the diff and ask for confirmation before running `-update`.

6. Never widen perf budgets or make tests less strict without explicit confirmation.
