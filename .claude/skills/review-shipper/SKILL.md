---
name: review-shipper
description: Review a Quesma Shipper change (src/, root Makefile, scripts/, shipper workflows, shipper docs) the way this repository's maintainers review - fail-closed scrub and seal, dedup and local-state semantics, installer and service lifecycle on a real desktop, compatibility surfaces and golden vectors, docs that state exactly what is guaranteed. Use for /review-shipper <PR | branch | path>, or when asked to review shipper code before a PR is marked ready.
argument-hint: "[PR number | branch | path]"
allowed-tools:
  - Read
  - Grep
  - Glob
  - Bash(git diff:*)
  - Bash(git log:*)
  - Bash(git show:*)
  - Bash(git merge-base:*)
  - Bash(git status:*)
  - Bash(gh pr view:*)
  - Bash(gh pr diff:*)
  - Bash(gh pr checks:*)
  - Bash(gh issue view:*)
  - Bash(go test:*)
  - Bash(go vet:*)
---

# Review a shipper change

The shipper collects AI-agent transcripts on a developer's machine, scrubs them, seals them with
age and uploads them straight to object storage with a presigned ticket. It then runs unattended
for months on machines nobody is watching. Review for what a maintainer would want fixed before
merge, not for a redesign. The design authority is `CONSTITUTION.md`, the layering is
`ARCHITECTURE.md`, the commit gate is `make check`.

## 1. Resolve the target

`$ARGUMENTS` is one of:

- A number: a pull request. `gh pr view <n> --json title,body,headRefOid,baseRefName,isDraft,files`
  and `gh pr diff <n>`. The base is `origin/<baseRefName>`.
- A branch name: `git diff $(git merge-base origin/main <branch>) <branch>`.
- A path: that path's diff against `origin/main`, plus uncommitted changes under it.
- Nothing: the commits ahead of the upstream plus uncommitted changes (`git log @{u}..`,
  `git diff @{u}`).

Record the head SHA you review; the summary names it.

Shipper territory is `src/`, the root `Makefile`, `scripts/`, `.github/workflows/quesma-shipper*.yml`
and the root documents that describe the shipper. When most changed files are under
`fleet-manager/`, stop and say the diff belongs to `/review-fleet-manager`. A PR that spans both
components gets both skills, each on its own files, plus one check that the two sides agree on the
wire contract (pass 4).

## 2. Before reviewing

Read, in this order: `AGENTS.md`; `CONTRIBUTING.md`, especially "Reviewers check these invariants"
and "Golden tests and compatibility"; `CONSTITUTION.md`; `ARCHITECTURE.md`, the import table and
"The invariants that outrank the tree"; `.github/PULL_REQUEST_TEMPLATE.md`. Then the PR description
and every comment already on it: `gh pr view <n> --comments`, and the inline threads with
`gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/comments --paginate`. A finding someone already
raised is not raised again. If it is still open at this head, leave it; if the new commits fixed
it, say so in the summary.

For every file the diff touches, read the base version with `git show origin/main:<path>` and
enough surrounding code to judge the change. A bug that was already there is pre-existing: list it
once at the end, never as a finding.

Load [checklist.md](checklist.md) for the complete checks per area. Load [gotchas.md](gotchas.md)
when the diff touches `src/packaging/`, a release workflow, the engine's state record or the dedup
path; it holds the platform facts reviewers had to supply before.

## 3. What counts as a finding

Only a problem this PR introduces, makes worse or newly exposes. Before reporting one:

1. Confirm the PR introduced or exposed it, against the base version.
2. Name a realistic path on which it happens, on a developer's machine, over time.
3. Name the concrete consequence: what uploads, what is lost, refused, left running or shown.
4. Point at the line.
5. Give the fix or its direction when it is short.

If you are not sure it is a bug, it is not a finding. A few findings you are sure of beat many you
are not. When the claim is about behaviour, reproduce it: run the package's tests, write a
throwaway test, or run the catalog through the engine against a synthetic store in the style of
`src/e2e`. The summary says what you ran and what you did not.

## 4. Review passes, heaviest first

Maintainers have spent their review time in this order. Do every pass whose area the diff
touches; skip the rest.

### Pass 1: the pipeline's promises

Constitution Article 4 and the ARCHITECTURE invariants are not negotiable, and no single test
checks all of them.

- Scrub fails closed: a scrub error means the file does not upload. Every new path from a source
  to the sealer goes through scrub. `transforms.Unscrubbed` is only for bytes that were never a
  transcript, such as compiled account and usage records; `scrub: false` is a compiled catalog
  property that runtime configuration cannot set.
- Opaque payloads are decoded or refused, never scanned raw. A `.zst`, gzip or SQLite file handed
  to a text scrubber reports a clean density and ships every credential inside it (#49).
- Seal always, to the recipients the served config names; the client never needs to decrypt.
- No cloud SDK and no new network path. Upload is a presigned PUT over `net/http`;
  `go list -deps ./cmd/quesma-shipper` shows nothing from `src/perf`.
- Writes go through `safeio` unless the package owns a durable artifact; the allow lists are
  `writeCapablePackages` and `writeCapableFiles` in `src/internal/platform/writepath_lint_test.go`,
  which also bans `os/exec` outside `src/packaging/` and the macOS Keychain reader. The shipper
  never writes inside an agent's store.
- A new import edge matches the table in `ARCHITECTURE.md` or is argued in the description.

### Pass 2: dedup and the local record (Articles 5 and 7)

A change near `fingerprints.json`, `already_present`, `source-hash`, `shipped-hash`, `state reset`
or `state prune` gets the scenario list: re-enrollment under a new install id, rollback to an
older binary, a scrub-rule change, recipient rotation (open as #77), a corrupt or foreign state
document.

- A foreign or corrupt document is discarded and rebuilt, with the archive answering what it
  already holds. It is never a refused run, and `doctor` explains it before `run` acts (#27, #33).
- Prune never claims another install's uploads. Reset may discard everything because it keeps
  nothing (#33).
- When a source keys objects on a logical identity rather than a path, the size-and-mtime shortcut
  is bypassed whenever the observed path differs from the recorded one, and among coexisting copies
  the newest wins, with plaintext only breaking ties (#49).
- `already_present` on the pre-scrub hash alone keeps an object scrubbed under old rules or sealed
  to old recipients. The description says what a reset does after a policy change (#74, #79).
- The local record is committed only after the destination confirms the write. That is what makes
  a hard kill on Windows safe, so nothing may write state before the PUT succeeds.

### Pass 3: installers and the service, on a desktop for months

Two installer PRs drew more review than everything else combined (#7, #53). CI cannot see what
reviewers ask here.

- Supervision: what restarts the agent if it dies, whether there is crash backoff, whether the
  budget resets after healthy uptime, whether the intentional-restart exit code is read, and
  whether the configured restart policy can engage at all.
- Nothing is visible on the user's desktop. Stdout and stderr land in `agent.out.log` and
  `agent.err.log` under the spec's `LogDir`, and the log is open before the first check that can
  fail; otherwise a silent restart loop is the only symptom (#7, #53).
- Self-update: the loop guard is persisted, compared against the running `build.Version`, and
  clears itself when the hop lands; the re-exec path is captured at startup; the install kind
  decides deliberately, a Homebrew cask keeps signed self-updates while a system-managed macOS
  install refuses them with `ErrSystemManaged` (#7, #36, #53, #82).
- The installer never reports success before the work is done. Uninstall is always possible,
  including from a mixed personal-plus-system install, aborts only when the thing to remove is
  confirmed present, is not raced by the process it deletes, and removes self-update leftovers
  (#7, #53, #59).
- Every refusal names a remedy, and the remedy works through the same guard that refused. A
  managed install is never told to run a command it refuses (#33, #53).
- `doctor` and `status` rows check what they claim: loaded is not running, registered is not
  alive, updated on disk is not restarted. A new failure mode gets a row.
  `TestAdvisoryRowsNeverFail` pins that warnings do not change the exit code; changing that is a
  human decision.
- The description lists live validation with versions: a personal install, an upgrade from the
  current release, a root or MDM install, and removal. CI smoke tests assert liveness, not
  registration.

### Pass 4: compatibility surfaces

- File formats, the wire protocol, configuration keys, the served document and the object-key
  grammar are compatibility surfaces. A change to one needs a test that old inputs still work, a
  note in the PR, and the order in which shipper, Fleet Manager and the ETL must upgrade (#49,
  #50, #62).
- Golden output in `src/e2e/golden_test.go`, the conformance vectors under `src/conformance/` and
  the wire fixtures behind `src/internal/controlplane/wire_contract_test.go` change only by hand,
  as additions explained in the description. `-update` is never run without a maintainer's
  agreement, and a vector whose format would change is deferred for confirmation (#50, #66, #69,
  #76).
- Removing or renaming a source id means a served config that still names it is rejected in full.
  The description names who must change first.
- A new `source=` value in object keys or a new record shape has consumers outside this repo. The
  description names the ETL and protocol follow-ups.

### Pass 5: docs say exactly what the system guarantees

The largest theme by count. Wrong documentation is a finding; wording preference is not.

- No "replaced" where a versioned bucket keeps the old version; no "cannot decrypt" where "cannot
  download" is meant; redaction is detection with configured rules, never a guarantee (#79).
- Every platform mentioned has an actionable link. Placeholders are variables that fail when
  unset, never example values that work. Agents nobody dogfoods are labelled experimental. Generic
  vendor instructions are cut (#7, #51, #52, #53).
- Every statement the PR makes stale is fixed in the same PR: schema notes under
  `src/internal/formats`, package comments, the READMEs under `src/packaging`, the root README (#79).

### Pass 6: code shape

- One owner per rule and one gate per operation. The same enumeration, template, absence check or
  validation appearing in shell and Go, in the personal and the system variant, or across the CLI,
  app and packaging layers is a finding that names the single definition (#33, #51, #53).
- Error text keeps the cause: distinct failures are not flattened into one message, an error from
  another context is not borrowed, a subprocess failure is not swallowed as "not found". Wrap with
  `%w` unless the wrapped error could carry a secret; then classify, and say so (#7, #53).
- Comments: one line for the genuinely unobvious corner case, none for what the code says, no
  narration, no rename churn.

### Pass 7: tests

- The test can fail, its name matches its assertion, and it runs on any host: no stat of
  `/Library/LaunchAgents`, no dependence on PATH, `runtime.GOOS` for platform branches (#53).
- Ask "does this test fail without the fix?" and expect the answer in the PR. A reproduction is
  answered with a named regression test (#49).
- A pinned invariant test is not weakened without a human decision: `TestAdvisoryRowsNeverFail`,
  the write-path and exec lint, the wire contract, the golden tests. Test removal is fine when the
  author names the test that still covers the behaviour (#60).

### Pass 8: security and privacy

- No secret reaches a log, an error or a report: grants, tokens, response bodies that echo request
  fields. Credential-bearing agent files stay deny-listed (#53, #64).
- A startup check that refuses the whole configuration runs only on globs the configuration added,
  never on compiled globs that can reach agent-written `.env` or key files (#64, #68).
- A read at a source boundary binds to the verified file: no symlink following, regular-file
  checks, the deny list applied to the resolved path, and a test that swaps the path between check
  and open (#51, #53).
- Any new `os/exec`, any permission the shipper or Fleet Manager newly needs, and any process that
  spawns the agent it watches is argued in the description and documented. Endpoint security
  tooling flags the last one (#38, #74).

### Pass 9: performance and size

- A change under scrub, seal, engine or gather comes with numbers: binary size in bytes with the
  delta, and the perf table with its budget and the CI-runner sample. Budgets are never widened
  (#49, #76).
- Per-file work placed before the seen check runs for every transcript on every tick (#51).

### Pass 10: fleet scale, product scope, and the PR itself

- Article 1: a behaviour that is fine on one machine and wrong on ten thousand is wrong, such as a
  check against `updates.quesma.dev` on every launch or a compiled-in collection ceiling. New
  sources are centrally configurable (#7, #51).
- Product-scope calls, such as supporting a new agent or holding a feature, are flagged with the
  trade-off for a maintainer, not decided by the review (#35, #51).
- The description opens with a before-and-after example, states compatibility in the author's
  words, lists what was not run, names what is deliberately not in this PR, and matches the final
  scope after a rebase (#33, #53). Deferred defects get an issue number in the thread; conflicting
  PRs name a merge order (#58, #59, #77).

## 5. Do not report

Formatting, vet and lint that `make check` enforces; naming unless it misleads; cosmetic renames;
test counts or coverage for their own sake; template checkboxes left unticked when the substance
is present; legacy or bridge code labelled temporary; edge cases with no realistic path; code the
PR does not touch; refactors the PR does not need. No praise, no restating the diff. Polish goes
under Notes, not as a finding.

## 6. Priorities

- **P1**, fix before merge: unscrubbed or unsealed bytes can upload; a credential or transcript
  reaches a log, an error or another machine; an installer or service that cannot be removed,
  restarts in a visible or tight loop, or loses the local record; a golden, conformance or wire
  change without justification; a widened perf budget or a weakened pinned test; a self-update
  loop at fleet scale.
- **P2**, should fix before merge: a real bug with a narrow trigger; a refusal with no working
  remedy; a doctor row that lies; a stale statement the PR made false; a compatibility note or
  upgrade order missing from the description; a missing test at the exact boundary a reproduction
  found.
- **P3**, worth a look: a concrete minor issue, a duplication with a named single owner, a comment
  the change made wrong. At most five; drop the weakest.

## 7. Report

Open with `Reviewed <sha>` and the counts per priority. Then one line per P1 and P2:
`[P1] path:line - what is wrong, when it happens, what it breaks, the fix if short`. Then:

- **Description check**: before-and-after example, compatibility statement, what was not run,
  scope matches title. One line each, present or missing.
- **Checked and holds**: at most five mechanisms you verified and how, by test run, reproduction or
  base comparison. Coverage, not praise.
- **Notes**: P3s and polish, at most five.
- **Pre-existing**: anything you found that the PR did not introduce, one line each, so it can be
  filed.

Post nothing to GitHub unless asked. When asked to comment, post each finding inline on its line
with the `[P1]`, `[P2]` or `[P3]` prefix and the summary as one PR comment. Never approve or
request changes on a maintainer's behalf.
