# Configure a private copy; grants must never be committed or included in deployment logs.
[CmdletBinding()]
param(
    [string]$Server = 'https://cp.example.com',
    [string]$Grant = 'REPLACE_WITH_ENROLLMENT_GRANT',
    [switch]$Remove
)

$ErrorActionPreference = 'Stop'
try {
    if (-not $Remove -and ($Server -eq 'https://cp.example.com' -or
        -not [uri]::IsWellFormedUriString($Server, [UriKind]::Absolute) -or
        ([uri]$Server).Scheme -ne 'https' -or ([uri]$Server).UserInfo -or
        [string]::IsNullOrWhiteSpace($Grant) -or $Grant -eq 'REPLACE_WITH_ENROLLMENT_GRANT')) {
        throw 'Set the HTTPS Server and enrollment Grant in your private script copy.'
    }
    $base = [Microsoft.Win32.RegistryKey]::OpenBaseKey(
        [Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryView]::Registry64)
    try {
        if ($Remove) {
            $key = $base.OpenSubKey('Software\Policies\Quesma\Shipper', $true)
            if ($key) {
                try { $key.DeleteValue('Grant', $false); $key.DeleteValue('Server', $false) }
                finally { $key.Dispose() }
            }
        } else {
            $key = $base.CreateSubKey('Software\Policies\Quesma\Shipper')
            try {
                $key.SetValue('Server', $Server, [Microsoft.Win32.RegistryValueKind]::String)
                $key.SetValue('Grant', $Grant, [Microsoft.Win32.RegistryValueKind]::String)
            } finally { $key.Dispose() }
        }
    } finally { $base.Dispose() }
    Write-Output 'Quesma Shipper managed enrollment policy updated.'
    exit 0
} catch {
    Write-Error 'Could not update managed enrollment policy. Check configuration and administrator permissions.'
    exit 1
}
