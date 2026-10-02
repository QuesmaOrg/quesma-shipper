# Windows installer validation

Run these scripts only on a disposable Windows VM from an elevated shell. They
register scheduled tasks and install or remove Quesma Shipper. Both support
Windows PowerShell 5.1.

`Test-ManagedTask.ps1` requires Windows Server and validates the task mechanism
independently of the shipper. An administrator registers a `BUILTIN\Users` group
task with an unrestricted logon trigger. A loopback RDP connection through the
built-in Windows control creates a real desktop session for a temporary standard
user. The test requires one logon-triggered child, then uses administrator
`RunEx` to start a second child in that session. Both children must report the
expected SID, profile, session, and ordinary token. The test restores the RDP
enablement setting and removes the temporary account, session, profile, and task.
Ordinary users receive read-only access to the task, matching production.

`Test-ManagedSetup.ps1` takes two installers and their release versions:

```powershell
./Test-ManagedSetup.ps1 -InstallerPath ./old/QuesmaShipperSetup-amd64.exe `
  -InitialVersion '0.0.0-456.abcdef' `
  -UpgradeInstallerPath ./new/QuesmaShipperSetup-amd64.exe `
  -ExpectedVersion '1.2.3-456.abcdef'
```

The test invokes installation and removal through temporary SYSTEM tasks. It
checks the installed files' permissions, native machine registration, task
configuration, collector identity and liveness, duplicate launches, refusal to
switch scope in place, repair, version upgrade, and retention of user state. It
exercises the upgrade recovery commands with initially enabled and disabled tasks,
checks Intune detection as SYSTEM, and checks that setup did not create shipper
state in service-account profiles. It corrupts the main executable and removes
the supervisor before uninstalling. With a file handle blocking main-executable
deletion, uninstall must return nonzero and preserve the uninstaller and exact
machine registration. Releasing the handle and retrying must remove the program
while retaining user state.

The Windows CI jobs run both scripts and retain the existing personal-installer
smoke test. These checks cannot establish all of the following behaviors.
Validate these on enterprise Windows VMs before rollout:

| Scenario | Expected result |
| --- | --- |
| SYSTEM installation with every interactive user logged out | Setup succeeds with no collector or service-account enrollment; first subsequent login starts collection as that user. |
| Standard local user, domain user, and Entra user | Each receives their own supervisor, enrollment, logs, and upload state with an ordinary token. |
| A user created after machine installation | First login starts collection without rerunning setup. |
| Two distinct users logged in concurrently | Both collect concurrently; restarting one user's collector leaves the other running. |
| One account logged in to two sessions | At most one collector owns that account's state; duplicate triggers do not leave competing supervisors. |
| Logoff, reconnect, supervisor crash, collector crash | No service-account fallback; collection recovers without duplicate uploads. |
| MDM upgrade with two users collecting | Both old instances stop; both restart from the replacement executable; upload history remains intact. |
| Failed or interrupted upgrade | Previously enabled startup is restored; a task disabled before the attempt remains disabled. |
| Personal installation in another or unloaded user profile | Setup or that user's first managed startup reports the scope conflict; there is no second collector and state is retained. |
| ARM64 Windows | Repeat installation, repair, upgrade, removal, and session tests with the ARM64 installer. |

Microsoft documents the [group logon trigger](https://learn.microsoft.com/en-us/windows/win32/taskschd/logontrigger-userid)
and [session-specific RunEx behavior](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-iregisteredtask-runex).
The tests check those semantics on Windows; cross-compilation does not validate
Task Scheduler or interactive session behavior.
