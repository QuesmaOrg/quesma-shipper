# Shipper review checklist

Every check here was asked for in a real review of this repository or is pinned by a document a
reviewer cited. Weight: **H** has blocked or reversed a merge, or recurs; **M** was asked for and
fixed before merge; **L** came up once. Numbers are quesma-shipper pull requests or issues where the
check was learned. "Codified" means `CONTRIBUTING.md`, `CONSTITUTION.md`, `AGENTS.md` or
`ARCHITECTURE.md` already states the rule; the rest is known only from review history.

Open the areas the changed paths map to, in the order given, run each check against the diff and
the base version, and report only what the PR introduced or made worse.

| Changed path | Areas, in order |
| --- | --- |
| `src/internal/formats/catalogdata/`, `src/internal/sources/` | C, J, D, H, K |
| `src/internal/transforms/`, `src/internal/transforms/packs/`, `src/conformance/` | A, D, K |
| `src/internal/engine/`, `src/internal/formats/fingerprint-state.schema.json` | B, A, D |
| `src/internal/config/`, `src/internal/controlplane/` | D, E, J |
| `src/internal/upload/`, `src/internal/platform/` | A, J |
| `src/packaging/`, `src/cmd/quesma-shipper-supervisor/` | F, G, H |
| `src/internal/cli/`, `src/app/` | G, F, B |
| `src/e2e/`, any `_test.go` | I, D |
| `src/perf/` | K |
| `README.md`, `RELEASE_DOWNLOADS.md`, `src/README.md`, `src/packaging/**/README.md` | H |
| `.github/workflows/`, `scripts/`, `Makefile`, `VERSION` | M |
| any renamed or redefined term | H.49 |

## A. Scrub, seal, upload, write path

1. **Scrub cannot be bypassed.** Grep the diff for `Unscrubbed(` and `scrub: false`. Every new use
   is a compiled account or usage source whose bytes were never a transcript, and the catalog entry
   says so. Runtime configuration never removes scrub or encrypt. [H, codified; #49, #76]
2. **Binary and compressed files do not reach the text scrubber as bytes.** The loader decodes
   `.zst` so it is scrubbed as plaintext; a source whose `sniff` kind is `jsonl` refuses a NUL byte
   in the head; a source with no sniff has only the catalog `exclude` list between a database,
   archive or compressed file and the scrubber. For a new tree of agent-copied files, check that
   list against what the tree realistically holds and name the gap when the list is the only
   guard. A compressed transcript scanned as text reports a clean density and ships every secret
   inside it. [H; #49]
3. **Seal always, to the served recipients.** No path writes plaintext to the sink. The client needs
   no decrypt capability. [H, codified]
4. **No cloud SDK, no second network path.** `go list -deps ./cmd/quesma-shipper` shows nothing from
   `src/perf`; upload stays a presigned PUT with exact-key validation and the origin allowlist in
   `src/internal/upload`. [H, codified]
5. **Write-path discipline.** A new direct `os` write is in `writeCapablePackages` or
   `writeCapableFiles` in `src/internal/platform/writepath_lint_test.go`, or it goes through
   `safeio`. A new `os/exec` import is under `src/packaging/` or the Keychain reader. Nothing
   writes inside an agent's store. Widening either allow list is a decision the description
   argues. [H, codified]
6. **Import edges.** Each new edge is in the `ARCHITECTURE.md` table; `sources` and `transforms`
   never import `engine`; `platform` imports nothing internal; `platform` and `formats` keep small
   exported surfaces. [M, codified]
7. **Account collectors stay bounded.** Provider reads happen during collection only, credentials
   stay in memory, a bucket's content is fetched only after the engine has checked upload state, and
   a successful upload skips later fetches in that bucket. [M; #31]

## B. Engine state and dedup

8. **Run the scenario list.** For any change near `fingerprints.json`, `already_present`,
   `source-hash`, `shipped-hash`, `state reset` or `state prune`: re-enrollment under a new install
   id; rollback to an older binary; a scrub-rule change; recipient rotation (#77 is open); a corrupt
   or foreign document. The description answers each. [H; #27, #33, #74, #79]
9. **A foreign document is discarded and rebuilt.** Never a refused run. The flush condition for a
   discarded document is kept, and `doctor` can read the record without applying the guard that
   `run` applies, so it explains before anything acts. [H; #27, #33]
10. **Prune keeps entries, so it must refuse a foreign document; reset discards everything, so it
    may adopt one.** Article 5 is the reason. [H; #33]
11. **Logical identity versus path.** Where a source keys an object on something other than its path,
    the stat shortcut is bypassed whenever the observed path differs from the recorded one, and the
    newest of several coexisting forms wins, with plaintext only breaking a tie. Reproduce through
    the catalog and engine, not by reading the code. [M; #49]
12. **Hash semantics.** `source-hash` is the pre-scrub hash; `shipped-hash` covers the scrub half;
    recipient rotation is still open. Any `already_present` decision states which hash it trusts and
    why that is enough. [M; #74, #79]
13. **Commit after the destination confirms.** No state write precedes a successful PUT. This is
    what makes a Windows hard kill and a launchd process-group kill safe. [H, codified; #7]

## C. Sources and catalog

14. **New sources declare what the served document may adjust.** A configuration layer can add
    include globs and switch a source off, but a root outside the compiled catalog is rejected with
    "a new root requires a release" (`resolveRoots` in `src/internal/config/resolve.go`). A new
    source therefore declares every root an operator could plausibly need, and the description says
    which parts the control plane can change without a release. [M; #51]
15. **Deny list on the resolved path, before existence.** A root pointed into a secrets directory is
    refused whether or not it exists. The whole-configuration deny check (`pickRoot` in
    `src/internal/config/resolve.go` calling `CheckIncludes` in `src/internal/sources/deny.go`)
    runs on include globs a configuration layer added, never on compiled globs over names the
    agent chooses; a catalog diff never shows this code, so open it for any new compiled glob over
    an agent-named tree. [M; #51, #64, #68]
16. **One collector per agent, shared helpers not copied.** A copied walk helper has already drifted
    by the time it is reviewed; an all-agents collector full of switches is a shape finding. [M; #51]
17. **Symlink and check-then-open races.** Opening with no symlink following, regular-file checks,
    and a test that replaces the path between check and open. [M; #51, #53]
18. **Hot-path order.** Discovery (`walkGlobs` in `src/internal/sources/gather.go`: deny,
    repository filter, sampled sniff) runs for every candidate on every tick; the fingerprint check
    runs later in the engine. Per-file work added to discovery runs for every transcript on every
    tick. [M; #51]
19. **Credential-bearing agent files are deny-listed.** Agent credential stores, token-bearing
    config files, local state databases and secret directories never match an include glob. Check
    the files the author names against the deny entries; completeness is the author's claim, so
    ask what else the tree holds. [M; #64]
20. **Agents the team does not run itself are labelled experimental** in the README; whether anyone
    runs it is not in the repository, so ask when the description does not say. [L; #51]

## D. Configuration, served document, compatibility, golden output

21. **Compatibility surfaces.** File formats, the wire protocol, configuration keys, the served
    document and the object-key grammar. A change needs a test that old inputs still work and a
    note in the PR. [H, codified]
22. **Golden and conformance vectors change as agreed additions.** They are regenerated by the
    `-update` flags in `src/e2e/golden_test.go`, `src/internal/formats/conformance_test.go` and
    `src/internal/transforms/scrub_conformance_test.go`, only after a maintainer has agreed to the
    diff; the description explains every added or changed vector; a vector whose format would
    change is deferred. The wire fixtures behind `src/internal/controlplane/wire_contract_test.go`
    change only through a shipper-protocol release. [H, codified; #50, #66, #69, #76]
23. **Source ids in both directions.** A served document naming an id the running binary lacks is
    rejected in full and collection continues under the last valid configuration (`RejectionError`
    in `src/internal/config/resolve.go`). Adding a source means an older client rejects any served
    document that adjusts it until it upgrades; removing or renaming one means a served document
    still naming it is rejected. The description names which side changes first. [M; #49, #50,
    #62, #64]
24. **Served-document authority.** A field the shipper-protocol rulebook says the control plane may
    not set is still refused; `src/internal/config/authority_rulebook_test.go` covers it. [M,
    codified]
25. **Cross-repo consequences are named.** New `source=` values need an ETL follow-up;
    `already_present` semantics need protocol wording; Fleet Manager decodes records strictly, so a
    new field needs an upgrade order. [M; #27, #49, #65]

## E. Control-plane client

26. **Enrollment retries resend the saved exact body.** Fleet Manager treats a repeated grant
    enrollment as the same only when the body is byte-identical, so a hostname change between
    attempts conflicts forever (#58 is open). A change here does not worsen that. [M; #53]
27. **A revoked install says so and stops retrying** (#32 is open). A change near refresh or enroll
    states what a revoked install now does. [L; #32]
28. **Ticket use.** One key, inside this install's own prefix, exact-key validation, no header
    beyond the ticket contract. [M, codified]

## F. Packaging: installers, services, self-update

29. **Supervision.** What restarts the agent when it dies; crash backoff that resets after healthy
    uptime; the intentional-restart exit code is read by the supervisor; the configured restart
    policy can actually fire. [H; #7, #10]
30. **Nothing visible.** No console window, no dialog, no notification on the user's desktop. [H; #7]
31. **Logs exist before anything can fail.** `agent.out.log` and `agent.err.log` under the spec's
    `LogDir`, opened per launch, before the first gate that can refuse. A system LaunchAgent without
    `StandardErrorPath` relies on the process opening its own log. [H; #7, #53]
32. **Self-update guard.** Persisted in the state directory, compared against the running
    `build.Version`, cleared when the hop lands, with a test for "hop written, restart running the
    hopped-to version, update allowed again". Check the other platforms, since the update path is
    shared. The re-exec path is captured at startup, because the swap can delete the running
    binary's path. [H; #7, #82]
33. **The install kind decides self-update deliberately.** A Homebrew cask declares `auto_updates`
    and keeps signed self-updates; a system-managed macOS install refuses with `ErrSystemManaged`
    and points at the package. The description states which applies. [M; #36, #53]
34. **Installer success means the work is done.** Post-install steps surface their exit code; a
    failed cleanup of an old install does not block a repair. [H; #7]
35. **Uninstall is always possible.** An uninstall whose verification is inconclusive still
    proceeds; the uninstaller is not raced by the process it deletes; self-update leftovers such as
    the renamed old binary are removed. [H; #7, #53]
36. **Mixed personal and system installs.** Detection keys on the running binary's own path, not
    on the existence of a plist; both the user and the administrator have a way out (#59 is open).
    [H; #53]
37. **Every refusal names a remedy that works.** Trace the suggested command through the guard that
    produced the refusal. A managed install is never told to run a command it refuses. [H; #33,
    #53]
38. **Doctor and status rows are truthful.** Each row checks the thing it names; a new failure
    mode gets a row; `TestAdvisoryRowsNeverFail` keeps warnings from changing the exit code, and
    changing that is a human decision. A silent skip inverts the meaning of the output. [H; #7,
    #18, #33]
39. **Package scripts.** User enumeration has one owner (`src/packaging/macos/local-users.sh`);
    multi-line tool output and empty trailing lines are handled; nothing prints noise into the
    Installer log; the one genuinely unobvious step has its one-line comment. [M; #53]
40. **Live validation is in the description.** A personal install, an upgrade from the current
    release, a root or MDM install, and removal, each with versions. CI smoke tests assert
    liveness, not registration. [H; #7, #10, #53]
41. **Platform facts.** Windows, macOS and Linux behaviours reviewers had to supply are in
    [gotchas.md](gotchas.md); check the diff against the relevant section. [M]
42. **MDM and Intune documentation.** A concrete three-step rollout, no generic vendor instructions,
    enrollment described per user installation rather than per machine, and the fact that removing
    a profile does not unenroll stated once. [M; #53]

## G. CLI, doctor, errors

43. **Error text keeps the cause.** Distinct causes are not flattened; an error from another
    context is not reused; subprocess failures are not swallowed as "not found", and stderr is
    captured when it matters. [M; #7, #38, #53]
44. **Wrapping policy.** `%w` by default. When the wrapped error could carry a secret, classify
    instead and say so in a comment. [M; #53]
45. **CLI surface changes** update `doctor` text, the README and the install scripts in the same PR.
    [L]

## H. Documentation

46. **Guarantee wording.** "Replaced" is wrong in a versioned bucket; "cannot decrypt" is not
    "cannot download"; redaction is detection with configured rules. [H; #79]
47. **Actionable per platform.** Every platform named in the README has a link a reader can act on;
    the download route map covers it. [M; #7]
48. **Placeholders fail closed.** Variables that fail when unset, never example values that would
    work if pasted. [M; #52]
49. **Statements the PR made stale are fixed in the PR.** For every term the PR renames, redefines
    or removes, `git grep -n -i -e '<term>' <head> -- . ':!*_test.go'`; each hit is updated here or
    named in the description with a reason. Past misses sat in schema notes, package comments, the
    packaging READMEs and the root README. Not in a follow-up. [H; #79]
50. **No filler.** Repeated sentences, generic vendor steps and docs for unshipped features are cut.
    [M; #52, #53]

## I. Tests

51. **The test can fail and says what it tests.** An assertion that cannot fail, or a name that
    promises more than the assertion, is a finding. [M; #53]
52. **Host-independent.** No stat of system paths, no PATH dependence, `runtime.GOOS` for platform
    branches. [M; #53]
53. **For a fix, does the test fail without it?** Expect the answer in the PR; a reviewer's
    reproduction is answered with a named regression test. [H; #49]
54. **Pinned invariants stay pinned.** `TestAdvisoryRowsNeverFail`, the write-path and exec lint,
    the wire contract, the golden tests, the perf budgets. [H, codified]
55. **Removal is covered elsewhere.** A deleted test is named with the test that still covers the
    behaviour; a second adversarial pass has restored tests before. [M; #60]
56. **Shape over coverage.** A test for the shape that matters, at the boundary that was found, is
    worth more than coverage. Never ask for tests to raise a number. [M; #7, #31]

## J. Security and privacy

57. **No secret in logs, errors or reports.** Grants, tokens, response bodies that echo request
    fields. Bodies are truncated and never echo request fields. [M; #53]
58. **New exec or permission is argued.** A new `os/exec`, a Fleet Manager grant the shipper now
    needs, or a process that spawns the agent it watches, with the endpoint-security consequence
    stated. [M; #38, #74]
59. **Test data is synthetic.** No real transcripts, prompts, databases, logs or bundles; example
    addresses and invented names. [M, codified]

## K. Performance and size

60. **Numbers for scrub, seal, engine and gather changes.** Binary size in bytes with the delta; a
    perf table with the budget, a laptop sample and the CI-runner sample. Without Docker the
    reviewer takes them from the description and says so. [H, codified; #49, #76]
61. **Budgets come from the runner.** A budget derived from a laptop fails on the shared runner;
    the convention is headroom over the runner sample. Never widen a budget to pass. [H, codified]
62. **State what was not run** when `make perf` or `make race` is irrelevant to the change. [M; #74]

## L. Description, scope, commits

63. **First screen is behaviour before and after**: a CLI transcript, a table over a lifecycle, or
    redacted-versus-shipped output. [H, codified; #33, #53]
64. **Compatibility in the author's words**, not the template checkboxes: which ids, keys or schemas
    changed, who upgrades first, rollback behaviour, "no golden bytes changed". [H, codified]
65. **Decisions surfaced**: "needs a human", "not in this PR", "found along the way". [M; #33, #49]
66. **Title and body match the final scope**, rewritten after a rebase that changed behaviour. [M;
    #33]
67. **Scope hygiene.** A defect found in review that is not this PR's job gets a stacked PR or an
    issue number in the thread; merge order with a conflicting PR is named; duplicates are closed
    with a pointer. [M, codified; #58, #59, #77]
68. **Commit shape.** One commit per kind for large mechanical changes, with "review commit by
    commit" guidance when a commit is a verbatim copy. [L; #52, #60]

## M. CI and release workflows

69. **Publishing jobs declare an environment** so reviewers gate them before the secrets exist.
    [M; #52]
70. **Release scripts handle tool quirks**: a CLI that prints an error body to stdout before
    failing, backticks in unquoted heredocs, symlinked targets that `find -type f` skips. [L; #14,
    #24, #41]
71. **VERSION changes only to start a release line**; the stamp comes from the release script. [L,
    codified]
