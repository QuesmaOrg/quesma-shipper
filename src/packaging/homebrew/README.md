# Homebrew distribution

`scripts/homebrew-cask.py VERSION ARTIFACT_DIRECTORY` renders the cask from this template and the
signed macOS binaries. The release announcement job runs it only after TUF publication succeeds,
then attaches `quesma-shipper.rb` to the GitHub release. The download URLs are immutable TUF target
paths; Homebrew checks their SHA-256. Homebrew itself does not verify TUF metadata.

The cask installs the raw signed and notarized binary in its Caskroom, links it on `PATH`, and runs
`postinstall` without sudo. The shipper registers its usual per-user LaunchAgent. There is one
supervisor per machine user, so switching from `.pkg` requires uninstalling it first, keeping state.

The cask declares `auto_updates true`. Caskroom installations use the same signed TUF self-updater
as native installs: it replaces the binary in place and re-executes it, preserving the Brew symlink
and launchd executable path. The Caskroom directory retains the initially installed version name;
`quesma-shipper --version` reports the running version. Normal `brew upgrade` skips auto-updating casks;
`brew upgrade --cask --greedy quesmaorg/tap/quesma-shipper` explicitly upgrades through Brew.
Brew upgrades stop the old service and register the new version; uninstall stops it and preserves all
local state. `quesma-shipper uninstall` removes the service but keeps the Homebrew command installed;
add `--purge` to delete local state too. Neither command invokes Brew or removes its managed payload.
The path check follows symlinks and supports nonstandard Homebrew prefixes, but expects Homebrew's
normal `Caskroom/quesma-shipper/VERSION/quesma-shipper` layout.

## First release

1. Push the `homebrew-tap` main changes containing its updater workflow. Enable Actions and permit
   the workflow's `GITHUB_TOKEN` to write contents on `main`.
2. Merge and publish the shipper release containing the lifecycle changes and cask asset.
3. Run the tap's **Update Quesma Shipper** workflow, or wait for its hourly check. It copies the
   release cask to `Casks/quesma-shipper.rb` and commits on `main`.
4. Validate the public install command on a clean Mac before advertising it in onboarding.

Do not publish a cask pointing at an older shipper: those binaries reject the Caskroom install path
and lack the Brew uninstall protection. No new signing credentials or cross-repository token is required.

## Validation

The Darwin tests cover package ownership and run the CLI from a simulated Caskroom through a symlink,
checking that automatic and manual updates reach TUF while uninstall preserves the Brew payload and
honors `--purge`. A separate subprocess test replaces a staged binary through the real raw updater, re-executes it, and verifies
that its command symlink and service entry still address the replacement.
`scripts/test-homebrew.sh` installs, upgrades, and removes a local cask on a disposable macOS CI runner,
checks launchd and state preservation, and removes quarantine from the staged unsigned CI binary
using a preflight step injected only into the temporary test cask.
Production casks keep Homebrew's normal Gatekeeper checks enabled.
