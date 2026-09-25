# Deploy Quesma Shipper with macOS MDM

Use the same signed, notarized
[`quesma-shipper-macos-universal.pkg`](https://updates.quesma.dev/download/quesma-shipper-macos-universal.pkg)
for personal and company installations. It supports macOS 13+, Apple Silicon and Intel.

| Installer destination | App and command | Service | Updates/removal |
| --- | --- | --- | --- |
| Install for me only | `~/Applications/Quesma Shipper.app`, `~/.local/bin/quesma-shipper` | `~/Library/LaunchAgents/com.quesma.shipper.plist` | User, including self-update; no admin rights |
| Install for all users of this computer | `/Applications/Quesma Shipper.app`, `/usr/local/bin/quesma-shipper` | `/Library/LaunchAgents/com.quesma.shipper.plist` | Administrator/MDM; installation requires admin rights |

An MDM agent runs Installer as root and targets the system volume:

```sh
/usr/sbin/installer -pkg /path/to/quesma-shipper-macos-universal.pkg -target /
```

The system package immediately starts a LaunchAgent for the current console user (including
directory-backed accounts) and in existing local users' GUI sessions.
Future users get the same agent at login. The collector runs with each user's permissions;
there is no root collector or LaunchDaemon. Each user has a separate identity, enrollment,
configuration and upload history in `~/.local/state/trajectory-shipper` by default, and reads
that user's source files. Administrator installation does not enroll root or collect other
users' files. Users who have not yet logged in will not appear in the fleet yet.

## Enrollment profile

1. In Agent Statement, create an enrollment grant for the intended organization. Set the
   expiry appropriate to your rollout; the package does not create or extend grants.
2. Copy [`com.quesma.shipper.mobileconfig`](com.quesma.shipper.mobileconfig). Replace the
   `Server` URL with the fleet-manager URL from your enrollment command, and replace `Grant`
   with the grant secret. Use a plist editor so XML special characters remain escaped.
3. Deploy the profile at computer/device scope. The template uses the standard
   `com.apple.ManagedClient.preferences` payload to force `Server` and `Grant` strings in
   the `com.quesma.shipper` preference domain. If using a custom-preference editor, set those
   two keys in that domain. Keep profile and payload identifiers stable when replacing the
   grant so MDM replaces the existing profile.
4. Deploy the package to the same computers. Either order works: an unenrolled agent reads
   managed preferences at startup and retries each minute if the profile arrives late, the
   server is unavailable or the grant is refused. It starts collecting after enrollment.

The agent reads managed preferences through CoreFoundation, including user and device
profiles. Ordinary `defaults write` settings do not authorize managed enrollment. An already
enrolled user keeps their existing device identity and organization when the profile changes
or disappears. Manual `quesma-shipper login --server URL TOKEN` remains available.

**Treat the profile as an enrollment credential, not a secret vault.** macOS users who receive
the profile can read its grant, and possession permits additional enrollments until the grant
expires or is revoked. Restrict profile distribution, use an appropriate expiry, and revoke
the grant if exposed. Revocation/expiry prevents new enrollments; already enrolled users keep
their separate device credentials. A user who first logs in after expiry or revocation needs
a replacement grant in the profile. Rotate the profile before expiry when future users need
to enroll; grant replacement does not reset existing users or upload history. Removing the
profile alone neither uninstalls nor unenrolls existing users.

## Jamf Pro

Upload the signed package to your distribution point and add it to a computer policy with
an **Install** action. Scope that policy to the intended computers. Use a computer configuration
profile to upload the customized `.mobileconfig`, or configure Application & Custom Settings
with preference domain `com.quesma.shipper` and the two string keys. Scope the profile to the
same group. Run the package policy through your usual enrollment or recurring-check-in trigger;
Jamf's root installer uses the system destination automatically.

For an upgrade, replace the package in the policy with the new signed release and run it on the
same computers. For removal, remove/exclude the deployment policy and enrollment profile, then
run [`uninstall.sh`](uninstall.sh) as a root policy script. This preserves users' local state.

## Kandji

Create a Custom App library item with the signed `.pkg` as its installer. Assign it to the
intended Blueprint. Add the customized `.mobileconfig` as a Custom Profile library item in the
same Blueprint. Kandji installs the package with administrative privileges. If configuring
an audit for enforcement, check both the app's `CFBundleIdentifier` (`com.quesma.shipper`) and
the release version `QuesmaShipperReleaseVersion` in `/Applications/Quesma Shipper.app/Contents/Info.plist`,
plus the presence of `/Library/LaunchAgents/com.quesma.shipper.plist`.

For upgrades, update the Custom App package and desired-version audit together. For removal,
remove/exclude the app and profile library items from the Blueprint first, then run
[`uninstall.sh`](uninstall.sh) as an administrator script so enforcement does not reinstall it.

## Ownership, migration and troubleshooting

System installations disable automatic and explicit self-update. `quesma-shipper uninstall`,
including `--purge`, and `service uninstall` refuse a system installation before stopping any
agent or removing any files. Administrators update with the package and remove with the root
script above. Removing the package keeps each user's local state for a later reinstall.

Mixed personal, Homebrew and system installations are unsupported. The package checks for
personal app bundles and LaunchAgents in existing local accounts before copying files, and a
personal/Homebrew installer refuses a system installation. First uninstall the previous
installation through its owner, without purging state; then deploy the new installation.
For system-to-personal migration, have an administrator remove the system package first.

Run these as the affected logged-in user, without `sudo`:

```sh
/usr/local/bin/quesma-shipper status
/usr/local/bin/quesma-shipper doctor
launchctl print "gui/$(id -u)/com.quesma.shipper"
tail -n 50 ~/.local/state/trajectory-shipper/logs/agent.err.log
```

The system agent writes startup and enrollment retry diagnostics into its user's state directory
under `logs/agent.err.log`, including before enrollment succeeds. No fleet entry usually means
that user has not logged in or enrolled yet. Check profile scope,
the `Server` URL, grant expiry/revocation, and server connectivity. Do not paste grant values
into support logs. A shared Mac appears as separate enrolled installations for separate users.
The MDM inventory import and ownership rules live in
[Agent Statement's MDM guide](https://github.com/QuesmaOrg/agent-statement/blob/main/docs/MDM-ENROLLMENT.md).

Profile format and installer domains follow Apple's
[managed-preferences payload](https://github.com/apple/device-management/blob/release/mdm/profiles/com.apple.ManagedClient.preferences.yaml)
and [Distribution XML reference](https://developer.apple.com/library/archive/documentation/DeveloperTools/Reference/DistributionDefinitionRef/Chapters/Distribution_XML_Ref.html).
