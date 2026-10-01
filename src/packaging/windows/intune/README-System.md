# Deploy the all-users Windows installer with Intune

Use this route for device provisioning, shared computers, and administrator-owned
updates. One installer per architecture supports both installation scopes. Intune
runs setup as SYSTEM; the installed collector runs separately as each interactive
user in its Windows logon context. The task uses `LeastPrivilege`: standard users
are not elevated, while Windows can retain elevation for the built-in Administrator
or administrators on hosts with UAC disabled. Enrollment, credentials, logs, and upload
history stay in that user's profile. No user needs to be logged in during setup.

For example, IT can install the shipper on 500 laptops before delivery. A developer's
collector starts when they sign in and enrolls using the managed policy. A second
user on the same laptop gets separate enrollment and upload history.

## Prepare the Win32 app

Obtain the approved release's `QuesmaShipperSetup-amd64.exe` or
`QuesmaShipperSetup-arm64.exe`. Create a private package directory containing:

```text
Install-System.ps1
Detect-System.ps1
Uninstall-System.ps1
QuesmaShipperSetup-amd64.exe
```

Use Microsoft's [Win32 Content Prep Tool](https://github.com/microsoft/Microsoft-Win32-Content-Prep-Tool)
on Windows to package the directory, with `Install-System.ps1` as its setup file.
Keep output outside that directory. Upload the `.intunewin` through **Apps > All apps
> Create > Windows app (Win32)**. Use separate assignments for x64 and ARM64.

Set these options:

| Setting | Value |
| --- | --- |
| Install behavior | **System** |
| Install command | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-System.ps1 -InstallerPath .\QuesmaShipperSetup-amd64.exe -Version RELEASE_VERSION` |
| Uninstall command | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Uninstall-System.ps1` |
| Detection | Custom script: a private copy of `Detect-System.ps1` with its `Version` default set to `RELEASE_VERSION` |
| Run detection as 32-bit process on 64-bit clients | **No** |
| Device restart behavior | **No specific action** |
| Requirements | Correct architecture; Windows 10 1809 or newer |

Replace `RELEASE_VERSION` with the exact release, such as `1.2.3-456.abcdef`, in both
places. Apply your organization's script-signing policy. The wrapper explicitly
passes `/ALLUSERS /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-` and requires a zero
installer exit code. Assign **Required** to a device pilot group, then expand after
validation. See Microsoft's [Win32 app configuration](https://learn.microsoft.com/en-us/intune/app-management/deployment/add-win32)
and [Autopilot pre-provisioning](https://learn.microsoft.com/en-us/autopilot/pre-provision).

Detection checks the exact registered release, shared executable and supervisor,
uninstaller, and enabled managed task configuration. It succeeds before first login
or enrollment. It does not run a collector or read a user's status as SYSTEM.
Enrollment and actual uploads must be verified separately.

## Deploy managed enrollment policy

Configure a private copy of [Configure-System.ps1](Configure-System.ps1) with your
HTTPS `Server` and enrollment `Grant`. Keep it outside the checkout. Deploy it as an
Intune platform script with **Run using logged-on credentials: No** and **Run in
64-bit PowerShell: Yes**. The script writes native 64-bit registry values:

```text
HKLM\Software\Policies\Quesma\Shipper
    Server    REG_SZ    https://cp.example.com
    Grant     REG_SZ    <organization enrollment grant>
```

The installer and policy may arrive in either order. User collectors retry when
policy or connectivity is unavailable. Existing successful enrollment and identity
are retained when the policy changes. Rotating a bootstrap grant therefore does
not move enrolled users to another organization. Use an explicit enrollment
migration process when changing organizations.

These values are readable by ordinary users. Intune packages and scripts are not
secret stores. Use a grant with the scope, expiry, and enrollment limit appropriate
to the deployment, and revoke or rotate it through the control plane. An expired grant prevents successful new enrollments; removing the policy stops
new policy-driven enrollment attempts. Neither action revokes existing user credentials. Grant values are kept out of process arguments and script output.
To remove the policy, deploy the same script with `-Remove` after withdrawing its
installation assignment. This preserves enrollment and collected upload history.

## Installation, upgrades, and scope changes

All-users setup installs into the native `%ProgramFiles%\Quesma Shipper`, updates
the machine PATH, and registers `\Quesma Shipper Managed`. Its logon trigger runs
`quesma-shipper-supervisor.exe --managed` as each interactive user, including users
created after installation. The supervisor resolves each user's own log and state
locations. Multiple user sessions can run concurrently; duplicate supervisors for
one user are prevented.

MDM owns upgrades and removal. Automatic and explicit self-update are disabled for
this installation. Deploy a new installer and change the required detection version
together. Setup disables task launches, stops managed collectors before replacing
files, and restores startup after installation. If an upgrade fails, setup attempts
to restore the previous task's enabled state. Rerun the all-users installer when
setup logs report that recovery failed. Interrupted uploads resume from user state.

The personal installer remains the default for a fresh interactive installation;
`/CURRENTUSER` explicitly selects it for unattended deployment. Its existing AppId,
uninstall registration, task naming, and self-updates are retained.

Before switching scope, uninstall the previous installation without purging state.
All-users setup refuses detected personal installations; user startup also checks
for conflicting personal integration. Uninstall each affected user's personal copy
in their user context first. Setup never runs an old user-writable binary with
administrator privileges. Existing user state is reused after the scope switch.
Do not target the same device with both the personal and System deployment routes.

Remove the **Required** assignment before assigning **Uninstall**. Machine removal
stops managed tasks and removes shared binaries, registration, and PATH integration.
It retains all users' enrollment and upload history. Managed enrollment policy has
its own lifecycle; remove it separately with `Configure-System.ps1 -Remove`.

## Validate the deployment

Start with a small Windows VM and Intune pilot. Check installation as SYSTEM before
any user logs in, standard-user sign-in, Entra/domain accounts, two concurrent users,
a second user created after setup, reboot, repair, upgrade, and uninstall. Verify a
personal-to-machine transition retains identity and upload progress. Test policy
before and after installation, unavailable networking, and an expired grant.

In each intended user's session, run:

```powershell
& "$env:ProgramFiles\Quesma Shipper\quesma-shipper.exe" status
& "$env:ProgramFiles\Quesma Shipper\quesma-shipper.exe" doctor
```

Confirm control-plane enrollment and a real upload from a configured source. The
presence of an enabled task proves installation, not collection health. Inspect
user logs under `%USERPROFILE%\.local\state\trajectory-shipper\logs` when needed.

The scripts support Windows PowerShell 5.1. `tests/Test-Deployment.ps1` includes
machine detection scenarios without installing software. Windows installer and
session tests cover lifecycle behavior; an actual Intune pilot is still needed for
tenant policy and application-control validation. Release signing is temporarily
disabled, so devices requiring a trusted publisher may block this release.
