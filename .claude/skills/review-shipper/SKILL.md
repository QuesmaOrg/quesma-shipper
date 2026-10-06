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
  - Bash(git grep:*)
  - Bash(git merge-base:*)
  - Bash(git status:*)
  - Bash(git cat-file:*)
  - Bash(git fetch origin pull/:*)
  - Bash(git archive:*)
  - Bash(tar -x:*)
  - Bash(mktemp:*)
  - Bash(gh pr view:*)
  - Bash(gh pr diff:*)
  - Bash(gh pr checks:*)
  - Bash(gh issue view:*)
  - Bash(gh api repos/QuesmaOrg/quesma-shipper/pulls:*)
  - Bash(go env:*)
  - Bash(go list:*)
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

- A number: a pull request.
  `gh pr view <n> --json title,body,state,mergedAt,isDraft,headRefOid,baseRefOid,baseRefName,mergeCommit,files`
  and `gh pr diff <n>`. The base is `baseRefOid`, never `origin/main`, which may already contain
  the PR. For a squash-merged PR the head tree is `mergeCommit.oid`. Record `state`: for a merged
  PR the findings are issues to file, not change requests.
- A branch name: `git diff origin/main...<branch>`; the base is the merge-base the three dots
  select.
- A path: that path's diff against `origin/main`, plus uncommitted changes under it.
- Nothing: the commits ahead of the upstream plus uncommitted changes (`git log @{u}..`,
  `git diff @{u}`).

Record the head SHA you review; the summary names it. Line numbers in findings are head line
numbers as `gh pr diff` shows them on the `+` side; a removed line is cited with its base number
and marked `(base)`.

Shipper territory is `src/`, the root `Makefile`, `scripts/`, `.github/workflows/quesma-shipper*.yml`
and the root documents that describe the shipper. Decide ownership by where the behaviour changes,
not by counting files: when the behaviour lives in `fleet-manager/` and the `src/` files only
follow it, stop and say the diff belongs to `/review-fleet-manager`. When behaviour changes on both
sides, review the `src/` files here, list the `fleet-manager/` files under "Handed to
/review-fleet-manager" in the report, keep the cross-contract check from pass 4 in this report, and
then run `/review-fleet-manager <target>` as its own review.

**Short path.** When the diff touches no packaging, state record, scrub or seal code, catalog,
configuration, protocol or golden file, say so, read only the `ARCHITECTURE.md` paragraph the diff
touches, and run passes 5, 6, 7, 8 and 10. Pass 8 runs on every diff: a one-line logging change
in `src/app/` or `src/internal/upload/` is exactly the diff that takes this path.

## 2. Before reviewing

Read once per session: `AGENTS.md`; `ARCHITECTURE.md`, the import table and "The invariants that
outrank the tree"; `.github/PULL_REQUEST_TEMPLATE.md`. Later in the session, grep them for the
terms in the diff and reread only those paragraphs. Read `CONSTITUTION.md` and the "Golden tests
and compatibility" section of `CONTRIBUTING.md` when the diff touches a compatibility surface, the
state record or a catalog.

Read the PR description now. Read the existing threads only after your own pass, so they do not
steer it, then reconcile:

```sh
gh pr view <n> --comments
gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/reviews --paginate --jq '.[] | "\(.commit_id[0:7]) \(.state) \(.user.login): \(.body)"'
gh api repos/QuesmaOrg/quesma-shipper/pulls/<n>/comments --paginate --jq '.[] | "\(.path):\(.line // .original_line) @\(.original_commit_id[0:7]) \(.user.login): \(.body)"'
```

`original_commit_id` says which commit a thread was raised on, which decides whether a later commit
answered it. A finding someone already raised is not raised again: list every thread under
"Already raised" with its state at this head, open, fixed in a named commit, or moved to an issue.

For every modified file, read the base version with `git show <base>:<path>`. For every changed
exported function or catalog field, find its callers at the base
(`git grep -n <Name> <base> -- src`): the consequence usually lives in a file the diff does not
touch. A bug that was already there is pre-existing: list it once at the end, never as a finding.
A pre-existing defect that this PR makes materially more likely to fire is a finding; say which half
is pre-existing.

Load [checklist.md](checklist.md); its first table maps changed paths to the areas to open. Load
[gotchas.md](gotchas.md); it holds the platform, release and engine facts reviewers had to supply
before, and it is short enough to read every time.

## 3. What counts as a finding

Only a problem this PR introduces, makes worse or newly exposes. Before reporting one:

1. Confirm the PR introduced or exposed it, against the base version.
2. Name a realistic path on which it happens, on a developer's machine, over time.
3. Name the concrete consequence: what uploads, what is lost, refused, left running or shown.
4. Point at the line.
5. Give the fix or its direction when it is short.

If you are not sure it is a bug, it is not a finding. A few findings you are sure of beat many you
are not.

Tests and reproductions run on the head you review, never on whatever the working tree has
checked out. `git cat-file -t <sha>` tells you whether the commit is local; if not,
`git fetch origin pull/<n>/head`. Then unpack it beside the repository and run from there:

```sh
mktemp -d                              # prints <dir>
git archive <sha> | tar -x -C <dir>
go test -C <dir>/src ./...
```

One command per line, so each matches a pre-approved rule. `./...` and not a package list: the
installer and supervisor tests under `src/packaging` and `src/cmd` are what pass 3 depends on.

`TestEveryCatalogSourceScrubsItsCanary` in `src/internal/engine` runs every catalog source through
scrub; run it for any catalog change. Numbers that need Docker (`make perf`) come from the
description; say so. A live behaviour on another operating system comes from the author's
validation list; say so. The summary says what you ran, on which SHA, and what you did not.

## 4. Review passes

The numbering is the all-time weight of each area in this repository's reviews, by consequence:
passes 1 to 3 hold the findings that have reversed merges, pass 5 holds the most frequent ones.
Run the passes whose areas the diff touches, in the order the checklist's path table gives for
those paths, and name the passes you skipped.

### Pass 1: the pipeline's promises

Constitution Article 4 and the ARCHITECTURE invariants are not negotiable, and no single test
checks all of them. For a PR that adds no path to the sealer, no import and no write, this pass is
four confirmations; say so in one line and move on.

- Scrub fails closed: a scrub error means the file does not upload. Every new path from a source
  to the sealer goes through scrub. `transforms.Unscrubbed` is only for bytes that were never a
  transcript, such as compiled account and usage records; `scrub: false` is a compiled catalog
  property that runtime configuration cannot set.
- Binary and compressed files do not reach the text scrubber as bytes. The loader decodes `.zst`
  so it is scrubbed as plaintext; a source whose `sniff` kind is `jsonl` refuses a NUL byte in the
  head; a source with no sniff has only the catalog's `exclude` list between a database, archive
  or compressed file and the scrubber. For a new tree of agent-copied files, check that list
  against what the tree realistically holds and name the gap when the list is the only guard. A
  compressed transcript scanned as text reports a clean density and ships every credential inside
  it (#49).
- Seal always, to the recipients the served config names; the client never needs to decrypt.
- No cloud SDK and no new network path. Upload is a presigned PUT over `net/http`;
  `go list -deps ./cmd/quesma-shipper` from `src/` shows nothing from `src/perf`.
- Writes go through `safeio` unless the package owns a durable artifact; the allow lists are
  `writeCapablePackages` and `writeCapableFiles` in `src/internal/platform/writepath_lint_test.go`,
  which also bans `os/exec` outside `src/packaging/` and the macOS Keychain reader. The shipper
  never writes inside an agent's store.
- A new import edge matches the table in `ARCHITECTURE.md` or is argued in the description; a
  grep of the diff's import blocks settles it.

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

Two installer PRs drew more review than everything else combined (#7, #53). Neither CI nor a diff
shows what reviewers ask here: reason from the code and from the live validation the description
reports, and say which.

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
- Golden output (`src/e2e/golden_test.go`), the conformance vectors (`src/conformance/`, regenerated
  by the `-update` flags in `src/internal/formats` and `src/internal/transforms`) and the wire
  fixtures behind `src/internal/controlplane/wire_contract_test.go` change only as additions the
  description explains, after a maintainer has agreed to the diff; a vector whose format would
  change is deferred for confirmation (#50, #66, #69, #76).
- Source ids are a surface in both directions. A served document that names an id the running
  binary's catalog lacks is rejected in full, and collection continues under the last valid
  configuration (`RejectionError` in `src/internal/config/resolve.go`). Adding a source therefore
  means an older client rejects any served document that adjusts it until it upgrades; removing or
  renaming one means a served document still naming it is rejected. The description names who
  must change first.
- A new `source=` value in object keys or a new record shape has consumers outside this repo. The
  description names the ETL and protocol follow-ups. When the PR spans both components, check
  that `src/` and `fleet-manager/` read the same shipper-protocol version and field names.

### Pass 5: docs say exactly what the system guarantees

The most frequent finding. Wrong documentation is a finding; wording preference is not.

- No "replaced" where a versioned bucket keeps the old version; no "cannot decrypt" where "cannot
  download" is meant; redaction is detection with configured rules, never a guarantee (#79).
- Every platform mentioned has an actionable link. Placeholders are variables that fail when
  unset, never example values that work. Agents the team does not run itself are labelled
  experimental; ask when the description does not say. Generic vendor instructions are cut (#7,
  #51, #52, #53).
- Stale statements: for every term the PR renames, redefines or removes, grep the head tree
  outside tests and check each hit was updated or is named in the description with a reason.
  Schema notes under `src/internal/formats`, package comments, the READMEs under `src/packaging`
  and the root README are where past misses sat (#79):

  ```sh
  git grep -n -i -e '<term>' <sha> -- . ':!*_test.go'
  ```

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
- For a fix, ask "does this test fail without the fix?" and expect the answer in the PR. A
  reproduction is answered with a named regression test (#49).
- A pinned invariant test is not weakened without a human decision: `TestAdvisoryRowsNeverFail`,
  the write-path and exec lint, the wire contract, the golden tests. Test removal is fine when the
  author names the test that still covers the behaviour (#60).

### Pass 8: security and privacy

- No secret reaches a log, an error or a report: grants, tokens, response bodies that echo request
  fields (#53).
- Credential-bearing agent files are deny-listed. Check the files the author names against the
  deny entries; completeness of the list is the author's claim, so ask what else the tree holds
  (#64).
- The whole-configuration deny check (`pickRoot` in `src/internal/config/resolve.go` calling
  `CheckIncludes` in `src/internal/sources/deny.go`) runs only on include globs a configuration
  layer added, never on compiled globs over names the agent chooses. A new compiled glob over an
  agent-named tree is where this has gone wrong before, and the code that decides it is never in
  a catalog diff (#64, #68).
- A read at a source boundary binds to the verified file: no symlink following, regular-file
  checks, the deny list applied to the resolved path, and a test that swaps the path between check
  and open (#51, #53).
- Any new `os/exec`, any permission the shipper or Fleet Manager newly needs, and any process that
  spawns the agent it watches is argued in the description and documented. Endpoint security
  tooling flags the last one (#38, #74).

### Pass 9: performance and size

- A change under scrub, seal, engine or gather comes with numbers: binary size in bytes with the
  delta, and the perf table with its budget and the CI-runner sample. Budgets are never widened
  (#49, #76). Without Docker the numbers come from the description; say so.
- Discovery (`walkGlobs` in `src/internal/sources/gather.go`: deny, repository filter, sampled
  sniff) runs for every candidate on every tick; the fingerprint check runs later in the engine.
  Per-file work added to discovery runs for every transcript on every tick (#51).

### Pass 10: fleet scale, product scope, and the PR itself

- Article 1: a behaviour that is fine on one machine and wrong on ten thousand is wrong, such as a
  check against `updates.quesma.dev` on every launch. A root outside the compiled catalog is still
  rejected (`pickRoot` in `src/internal/config/resolve.go`; the protocol rulebook's note reads "a
  new root requires a release"), so a new source declares every root an operator could plausibly
  need, and the description says which parts the served document can adjust (#7, #51).
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
  reaches a log, an error or another machine; a configuration refusal that stops collection for
  every source on the machine; an installer or service that cannot be removed, restarts in a
  visible or tight loop, or loses the local record; a golden, conformance or wire change without
  justification; a widened perf budget or a weakened pinned test; a self-update loop at fleet
  scale.
- **P2**, should fix before merge: a real bug with a narrow trigger; a refusal with no working
  remedy; a doctor row that lies; a stale statement the PR made false; a compatibility note or
  upgrade order missing from the description when older clients would reject the served document;
  a missing test at the exact boundary a reproduction found.
- **P3**, worth a look: a concrete minor issue, a duplication with a named single owner, a comment
  the change made wrong. At most five; drop the weakest.

## 7. Report

Open with `Reviewed <sha>` and the counts per priority, or `No P1 or P2 findings.` Then one line
per P1 and P2: `[P1] path:line - what is wrong, when it happens, what it breaks, the fix if short`.
Then:

- **Already raised**: every existing thread, one line each, with its state at this head.
- **Description check**, one line each, present, partial or missing: before and after;
  compatibility in the author's words, with upgrade order; binary size and perf numbers when
  scrub, seal, engine or gather changed; live validation for packaging changes; what was not run;
  deliberately not in this PR; scope matches title.
- **Checked and holds**: at most five mechanisms you verified and how, by test run, reproduction or
  base comparison. Coverage, not praise.
- **Ran / not run**: tests and reproductions, with the SHA they ran on; passes skipped and why.
- **Handed to /review-fleet-manager**: the `fleet-manager/` files you left to the other skill, if
  any.
- **Notes**: P3s and polish, at most five.
- **Pre-existing**: anything you found that the PR did not introduce, one line each, so it can be
  filed.

Post nothing to GitHub unless asked. When asked to comment, post each finding inline on its line
with the `[P1]`, `[P2]` or `[P3]` prefix and the summary as one PR comment. Never approve or
request changes on a maintainer's behalf.
