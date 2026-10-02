# Shipper gotchas

Facts a reviewer had to supply in a past review, grouped by where they bite. Each one cost a review
round; knowing it lets the review start from the fix. Numbers are the pull requests where the fact
surfaced.

## Windows

- A Task Scheduler console action under `InteractiveToken` shows a visible window; `<Hidden>` only
  hides the task in the listing. The supervisor is a Go binary built with `-H windowsgui`. (#7)
- `timeout.exe /t /nobreak` fails with "Input redirection is not supported" when stdin is not a
  console, so a wrapper that hides the window turns a sleep loop into a tight spin. (#7)
- An environment-variable guard dies with the process; the supervisor relaunches from a fresh
  environment. Loop guards live in the state directory, are compared against `build.Version`, and
  clear themselves when the hop landed. (#7)
- `schtasks` returns exit code 1 for a missing task, access denied, malformed arguments and a dead
  service alike. Prove absence by enumerating with `/Query /FO CSV /NH` and read stdout only, since
  a corrupt task prints warnings on stderr. (#7)
- The self-update library renames the running binary to `.quesma-shipper.exe.old` and can only
  hide it on Windows. Inno Setup removes only files it installed, so `[UninstallDelete]` names the
  leftover. (#7)
- Windows paths compare case-insensitively; a plain string comparison of the task's program against
  `os.Executable()` makes uninstall skip the task. Use `packaging.SameProgram`. (#7)
- `ISCC.exe` carries no usable version resource; `Compil32.exe` does. A version probe is advisory
  and never fails the build on "could not tell". (#7)
- Task Scheduler rejects `<Duration>P0D</Duration>`; omitting `Duration` is what repeats forever.
  `<RestartOnFailure>` fires only when the action exits non-zero, so an action that loops forever
  makes it dead configuration. (#7)
- Windows delivers no SIGINT or SIGTERM and `schtasks /End` is a hard termination, so there is no
  graceful drain. This is safe only because the local record commits after the destination confirms
  the write; the package comment in `src/packaging/windows/task.go` says so. (#7)
- Inno `[Run]` ignores exit codes: a failed post-install step shows a green check unless it runs
  from `[Code]` through `Exec`. (#7)
- The update channel replaces `quesma-shipper.exe` only; the supervisor is frozen at install time,
  so its restart policy must be conservative, because a permanent give-up can only be undone by
  re-running setup. (#7)
- Go's monotonic clock steps about half a millisecond on the Windows runner; a small scrub can
  measure zero. (#48)
- An npm `.cmd` launcher needs a command interpreter; killing the immediate process leaves a
  descendant holding the stdout pipe, and `WaitDelay` does not unblock that read. (#38)

## macOS

- launchd kills a job's whole process group when its main process exits, so a `launchctl bootout`
  started as a child of the exiting agent never runs. (#10)
- `dscl` puts a value containing spaces on its own line, and `dscl . -list /Users NFSHomeDirectory`
  returns every user in one call. A numeric test on the empty trailing line prints "integer
  expression expected" into the Installer log. (#53)
- `os.Hostname()` changes with the network. Fleet Manager treats a repeated grant enrollment as the
  same only when the whole body is byte-identical, so a retry after a lost response must resend the
  saved exact body. (#53, #58)
- A system-managed check keyed on the existence of the system LaunchAgent plist is true for a
  personal binary too; mixed installs are told apart by the running binary's own path. (#53)
- The shared system LaunchAgent plist has no `StandardErrorPath`, so a check that runs before the
  process opens `agent.err.log` fails invisibly and launchd restarts it every 60 seconds. (#53)
- A Homebrew cask can declare `auto_updates true`, so signed self-update is legitimate under
  Homebrew; dropping it there is a regression. (#36)
- Removing an MDM profile does not unenroll; the install keeps its identity and state. (#53)

## Linux

- Self-update deletes the renamed old binary, so `os.Executable()` after the swap points at a
  deleted path. Capture the path at startup. (#82)
- The installer puts the binary under the user's local bin and registers a systemd user service;
  running it as root is refused. (`CONTRIBUTING.md`)

## Release and CI

- tuftool creates symlinks under the TUF targets directory; `find -type f` skips them, `find -L`
  does not. (#14)
- `gh api` prints the error JSON to stdout before failing; `|| true` turns that body into a "tag
  SHA". (#24)
- Release notes written through an unquoted heredoc treat a bare backtick as command substitution.
  (#41)
- GitHub environment reviewers engage only on jobs that declare `environment:`; a publish job
  without one pushes an image or a release unreviewed once its secrets exist. (#52)
- Container registries move: a pinned image tag must be re-verified when a workflow or compose file
  changes it. (#15, #22)

## Engine, configuration and state

- A served document that names a source id the running binary's catalog lacks is rejected in
  full, and collection continues under the last valid configuration. Every new agent therefore
  needs an upgrade-order note: older clients reject a served document that adjusts the new source
  until they upgrade. (#64)
- Compiled include globs over agent-named trees can reach files named like secrets. The
  whole-configuration deny check runs on configuration-added globs only, and lives in
  `src/internal/config/resolve.go` and `src/internal/sources/deny.go`, which a catalog diff never
  shows. (#64, #68)
- A root outside the compiled catalog is rejected with "a new root requires a release"; a
  configuration layer can add include globs and switch a source off, not add a root. (#51)

- The size-and-mtime stat shortcut is unsafe when one logical identity spans several paths, such as
  a live transcript, its archived copy and its compressed copy. Read and hash whenever the observed
  path differs from the recorded one. (#49)
- "Prefer plaintext" can select a stale copy; pick by freshness, with plaintext breaking ties. (#49)
- `source-hash` is the pre-scrub hash, so `already_present` on it alone keeps an object scrubbed
  under old rules; `shipped-hash` fixes the scrub half. Recipient rotation is open as #77. In a
  versioned bucket a "replaced" object keeps its earlier versions. (#74, #79)
- A foreign `fingerprints.json` is discarded and rebuilt, with the archive answering
  `already_present`; it is never a refused run. Reading the record for `doctor` does not apply the
  guard that `run` applies, so `doctor` explains first. (#27, #33)
- Doctor's warning rows never change the exit code; `TestAdvisoryRowsNeverFail` pins it, so a fully
  stopped install still exits 0. Changing that is a human decision. (#33)
- Conformance vectors and golden output are regenerated with `-update` flags in `src/e2e`,
  `src/internal/formats` and `src/internal/transforms`, only after a maintainer has agreed to the
  diff; a vector that would change format is deferred for confirmation. (#50, #62, #66, #69, #76)
- Fleet Manager decodes `tags.json` strictly: an older replica drops any record that carries a new
  field, so a new field needs every replica upgraded first. (#65)
