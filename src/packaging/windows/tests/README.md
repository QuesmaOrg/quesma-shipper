# Windows installer validation

Run these scripts only on a disposable Windows VM from an elevated shell. They
register scheduled tasks and install or remove Quesma Shipper. Both support
Windows PowerShell 5.1.

`Test-ManagedTask.ps1` validates the task mechanism independently of the shipper:
an administrator registers a `BUILTIN\Users` group task with an unrestricted logon
trigger, then `RunEx` starts two parallel instances as a temporary standard user
with a logon token in the current session. Each child reports its SID, user
profile, session, and elevation. The test requires an ordinary token and rejects
service-account identities. A separate account is necessary because hosted
runners use the built-in Administrator, for which Windows ignores `RunLevel`.
The prototype grants the test user execute access for `RunEx(AS_SELF)`. The real
managed task grants ordinary users read access only; the installer test checks
that restriction and starts it with administrator-controlled session targeting.

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
state in service-account profiles.

The Windows CI jobs run both scripts and retain the existing personal-installer
smoke test. Its one logged-in administrator session cannot establish all of the
following behaviors. Validate these on enterprise Windows VMs before rollout:

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
