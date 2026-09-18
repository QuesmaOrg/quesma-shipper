# Deploy Quesma Shipper with Microsoft Intune

Deploy the existing **per-user setup executable** and enroll each Windows user with
the control plane. Intune supports user-context installation; a machine-wide
installer is not required for this workflow. Installation and `login` must run as
the same user, whose profile contains the coding-agent files to collect.

Use the **platform-script route** for a small pilot you administer from a Mac. Use
the **Win32 app route** for app detection, required assignments, and managed
uninstallation. Both use [Install.ps1](Install.ps1), including unattended login.
Deploy only one route to a given pilot group.

## Before deployment

- Start with a small Microsoft Entra **joined** or hybrid-joined, Intune-enrolled
  Windows group, for example `Example Organization - Shipper pilot`. Have the
  intended user signed in for the pilot. This guide does not cover Entra-registered
  personal devices, multi-session Windows, or installation before user sign-in.
- Obtain `QuesmaShipperSetup-amd64.exe` or `QuesmaShipperSetup-arm64.exe` from the
  [Windows releases](../../../../README.md#from-a-release). The raw
  `quesma-shipper-windows-<arch>.exe` does not install the supervisor or scheduled task.
  Use separate deployments for x64 and ARM64 devices.
- Know the control-plane HTTPS URL, exact organization name returned by `login`,
  and an enrollment grant valid for the pilot's number of installations. A
  single-use invite cannot enroll a group of users.
- Allow access to the installer download, control plane, configured upload
  destinations, and update service. Confirm the device's application-control policy
  permits the release; Windows code signing is currently disabled in the release
  workflow. This guide does not bypass that policy.

Copy the scripts to a **private directory outside your repository checkout** before
editing. Examples here use only `example.com`, `Example Organization`, and obvious
placeholders. Never commit configured scripts, grants, deployment packages, or logs.

The sample embeds a bootstrap grant in the private script for unattended enrollment.
Intune scripts and packages are not secret stores: treat that grant as disclosed to
their administrators and recipients. Use an organization-approved, limited enrollment
grant and revoke it after the pilot's enrollments finish, according to your control
plane's policy. If your organization forbids embedding grants, arrange approved
credential delivery before using this recipe. Passing it via an environment variable
keeps it out of the login command line; it does not conceal the private script itself.

## Route A: platform script, prepared on macOS or Windows

No Windows packaging utility is needed. The Mac prepares a text file and uploads it;
Intune executes PowerShell on the target Windows devices.

### Prepare the script

Copy [Install.ps1](Install.ps1) outside the checkout. Edit these defaults at the top
using a plain-text editor, keeping the `.ps1` extension:

| Setting | What to enter |
| --- | --- |
| `Server` | Your control-plane URL, replacing `https://cp.example.com` |
| `Organization` | The exact organization name reported by the control plane |
| `Grant` | Your enrollment grant, replacing `REPLACE_WITH_ENROLLMENT_GRANT` |
| `InstallerUrl` | An HTTPS URL to the setup executable that the device can download without an interactive sign-in |
| `InstallerSha256` | The SHA-256 of that exact approved setup executable |
| `InstallerPath` | Leave empty for this route |

Prefer a version-specific download URL. If a moving download URL starts returning a
new release, the hash check fails until you approve that release and update the hash.
If a value contains a single quote, double it inside the PowerShell single-quoted string.

Calculate the hash of your approved installer on your Mac:

```sh
shasum -a 256 ~/Downloads/QuesmaShipperSetup-amd64.exe
```

Or on Windows:

```powershell
(Get-FileHash .\QuesmaShipperSetup-amd64.exe -Algorithm SHA256).Hash
```

Copy only the 64-character hash. The script checks the downloaded bytes before
executing them. Obtaining the hash from your approved artifact establishes which
bytes you intend to deploy; a hash is not a substitute for trusting that artifact.

### Click through Intune

1. Open **Devices > Scripts and remediations > Platform scripts > Add > Windows 10
   and later**.
2. Name it `Quesma Shipper - user pilot`.
3. Upload your configured `Install.ps1` and set:
   - **Run this script using the logged on credentials: Yes**.
   - **Enforce script signature check: No** for the unsigned sample, where permitted;
     otherwise sign your configured copy and select **Yes**.
   - **Run script in 64-bit PowerShell host: Yes**.
4. Keep appropriate scope tags. Under **Assignments**, include your existing pilot
   device group.
5. Review and select **Add**. Monitor the policy's **Device status** and **User status**.

Microsoft documents these options in
[Use PowerShell scripts on Windows devices](https://learn.microsoft.com/en-us/intune/device-management/tools/run-powershell-scripts-windows).
A successful script does not run continuously. Failures receive three retries at
subsequent management-extension check-ins. After those are exhausted, fix the cause
and upload a changed script to trigger another attempt. Deleting a policy does not
uninstall an installed shipper.

## Route B: Win32 app with installation and enrollment detection

The Intune portal works on macOS. The `.intunewin` packaging step requires a Windows
computer, VM, or runner with Microsoft's
[Win32 Content Prep Tool](https://github.com/microsoft/Microsoft-Win32-Content-Prep-Tool).

### Prepare the package on Windows

Create a private source folder containing:

```text
C:\QuesmaIntune\Source\
    Install.ps1
    Uninstall.ps1
    QuesmaShipperSetup-amd64.exe
```

Configure `Server`, `Organization`, `Grant`, and `InstallerSha256` in `Install.ps1`
as above. `InstallerUrl` is unused because the install command supplies
`-InstallerPath`. Separately copy [Detect.ps1](Detect.ps1), and set its `Server` and
`Organization` to exactly the same values. Detection needs no grant.

Keep the packaging utility and output directory outside `Source`. From the directory
containing `IntuneWinAppUtil.exe`, run:

```powershell
.\IntuneWinAppUtil.exe -c C:\QuesmaIntune\Source -s Install.ps1 -o C:\QuesmaIntune\Output -q
```

Upload the resulting `Install.intunewin` from either Windows or your Mac. See
[Prepare Win32 app content](https://learn.microsoft.com/en-us/intune/app-management/deployment/create-win32-package).

### Click through Intune

1. **Apps > All apps > Create > Windows app (Win32) > Select**. Upload
   `Install.intunewin`. Name it `Quesma Shipper - user pilot`, publisher `Quesma`.
2. Under **Program**, select **Command line** and enter:

   Install command:
   ```text
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -InstallerPath .\QuesmaShipperSetup-amd64.exe
   ```

   Uninstall command:
   ```text
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Uninstall.ps1
   ```

   Set **Install behavior: User**, **Device restart behavior: No specific action**.
   In **Return codes**, add **1 = Retry** for this wrapper's failures. Unchanged bad
   configuration still needs an administrator to fix it.
3. Under **Requirements**, select the matching architecture and your supported
   Windows minimum. For ARM64, replace the bundled filename and install command.
4. Under **Detection rules**, choose **Use a custom detection script**, upload the
   configured `Detect.ps1`, set **Run script as 32-bit process on 64-bit clients: No**,
   and set signature enforcement according to your script-signing policy.
5. Leave dependencies and supersedence empty for a fresh pilot. Set appropriate scope
   tags. Under **Assignments > Required > Add group**, select your existing device
   group. **Review + create > Create**.

These settings follow
[Microsoft's Win32 app configuration](https://learn.microsoft.com/en-us/intune/app-management/deployment/add-win32).
Assignment selects recipients; **User** selects the installation account even when
the assignment targets a device group. See
[assign apps to groups](https://learn.microsoft.com/en-us/intune/app-management/deployment/assign-groups).

## What installation and login do

The script uses `%LOCALAPPDATA%\Programs\Quesma Shipper`, the existing installer's
default location. Setup creates this user's scheduled task. The shipper waits for
enrollment while the script invokes:

```powershell
# The script supplies SHIPPER_AUTH_KEY only for this child process.
& "$env:LOCALAPPDATA\Programs\Quesma Shipper\quesma-shipper.exe" login --server 'https://cp.example.com'
```

No grant appears in the command arguments. The script restores the previous process
environment afterward, then checks the endpoint, organization, identity, and enabled
task before reporting success. It does not collect data through Intune; collection
and encrypted uploads use the shipper's existing control-plane configuration.

Retries reuse a complete installed copy rather than replacing a self-updated binary.
An existing enrollment in another organization or at another endpoint is an error;
these scripts never log it out or silently move it. If both organizations have the
same display name at the same endpoint, these checks cannot distinguish them: verify
the enrollment in the control plane. Custom installation directories are outside
this recipe.

The detection script requires enrollment and a registered task, but tolerates a
disabled task: service health is checked separately during the pilot. It does not pin the
installed binary's version, because existing self-updates remain enabled. These
scripts bootstrap enrollment; they do not give Intune ownership of shipper upgrades.

## Verify the pilot and remove it

On the target Windows device, in the intended user's PowerShell session:

```powershell
& "$env:LOCALAPPDATA\Programs\Quesma Shipper\quesma-shipper.exe" status
& "$env:LOCALAPPDATA\Programs\Quesma Shipper\quesma-shipper.exe" doctor
```

Check the organization, endpoint, scheduled task and control-plane enrollment. Create
a small test session in a configured source and confirm an upload, then sign out and
back in to check task startup. Intune success means setup and login completed; it does
not prove that collection has found files or that an upload has reached the sink.

Test a rerun after enrollment and a failed enrollment before expanding the group.
For failures, inspect Intune's script/app status and the user's shipper logs (by
default `%USERPROFILE%\.local\state\trajectory-shipper\logs`). A hash mismatch,
wrong architecture, blocked unsigned program, expired grant, and missing network
access require different fixes. Never paste grants or unredacted logs into public
issues.

For Win32 removal, remove the group's **Required** assignment before assigning
**Uninstall**. For the platform-script route, remove the installation assignment and
deploy [Uninstall.ps1](Uninstall.ps1) as a separate user-context platform script.
Both preserve enrollment and local upload state, matching the existing uninstaller.
Removing a script assignment alone does not remove the application.

The scripts require Windows PowerShell 5.1 or later. An actual Intune pilot is still
required to validate tenant policies, standard-user execution, enrollment, and uploads.

For maintainers, `tests/Test-Deployment.ps1` checks parsing, enrollment-target guards,
and detection against a synthetic executable. It requires Go and PowerShell, runs in
Windows CI under PowerShell 5.1, and does not install software or contact a control
plane. It can also run locally with PowerShell 7 on macOS.
