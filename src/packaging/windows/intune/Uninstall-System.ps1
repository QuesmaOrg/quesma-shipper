# Remove shared binaries and startup integration; user enrollment and upload state are retained.
$ErrorActionPreference = 'Stop'

function Assert-ProtectedProgramPath([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
        throw 'Managed program paths must not be reparse points.'
    }
    $acl = Get-Acl -LiteralPath $Path
    $trusted = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
    if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -notin $trusted) {
        throw 'Managed program paths must be owned by administrators or SYSTEM.'
    }
    $descriptor = [Security.AccessControl.RawSecurityDescriptor]::new($acl.GetSecurityDescriptorBinaryForm(), 0)
    if ($null -eq $descriptor.DiscretionaryAcl -or $descriptor.DiscretionaryAcl.Count -gt 4096) {
        throw 'Managed program permissions are invalid or unrestricted.'
    }
    foreach ($rule in $acl.Access) {
        if ($rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            ($rule.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly)) { continue }
        if (([int]$rule.FileSystemRights -band 0x500D0156) -ne 0 -and
            $rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -notin $trusted) {
            throw 'Managed program files can be modified by a non-administrator; repair permissions before removal.'
        }
    }
}

try {
    $base = [Microsoft.Win32.RegistryKey]::OpenBaseKey(
        [Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryView]::Registry64)
    try {
        $key = $base.OpenSubKey('Software\Microsoft\Windows\CurrentVersion')
        try { $dir = Join-Path ([string]$key.GetValue('ProgramFilesDir')) 'Quesma Shipper' }
        finally { $key.Dispose() }
        $registration = $base.OpenSubKey('Software\Quesma\Shipper')
        try {
            if ($registration -and [string]$registration.GetValue('InstallDir') -ine $dir) {
                throw 'Managed registration points outside the supported installation directory.'
            }
            $registered = $null -ne $registration
        } finally { if ($registration) { $registration.Dispose() } }
    } finally { $base.Dispose() }
    $uninstaller = Join-Path $dir 'unins000.exe'
    if (-not (Test-Path -LiteralPath $uninstaller -PathType Leaf)) {
        if ($registered -or (Test-Path -LiteralPath (Join-Path $dir 'quesma-shipper.exe'))) {
            throw 'Repair the managed installation before uninstalling: its uninstaller is missing.'
        }
        $scheduler = New-Object -ComObject 'Schedule.Service'
        $scheduler.Connect()
        foreach ($task in $scheduler.GetFolder('\').GetTasks(1)) {
            if ($task.Name -eq 'Quesma Shipper Managed') {
                throw 'Managed startup remains without an uninstaller; repair installation before removal.'
            }
        }
        exit 0
    }
    foreach ($path in @((Split-Path $dir), $dir, $uninstaller)) { Assert-ProtectedProgramPath $path }
    $process = Start-Process -FilePath $uninstaller -Wait -PassThru `
        -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART'
    exit $process.ExitCode
} catch {
    Write-Error $_
    exit 1
}
