# Bundle Detect-System.ps1 and the approved architecture-specific installer with this script.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InstallerPath,
    [Parameter(Mandatory = $true)][string]$Version
)

$ErrorActionPreference = 'Stop'
try {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Use System install behavior or an elevated administrator session.'
    }
    if ($Version -notmatch '^\d+\.\d+\.\d+-\d+\.[0-9A-Za-z]+$') {
        throw 'Supply the exact release version bundled in the installer.'
    }
    $detect = Join-Path $PSScriptRoot 'Detect-System.ps1'
    if (-not (Test-Path -LiteralPath $detect -PathType Leaf)) {
        throw 'Bundle Detect-System.ps1 beside Install-System.ps1.'
    }
    $setup = Start-Process -FilePath (Resolve-Path -LiteralPath $InstallerPath).Path -Wait -PassThru `
        -ArgumentList '/ALLUSERS /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-'
    if ($setup.ExitCode -ne 0) { throw "Installer failed with exit code $($setup.ExitCode)." }
    $shell = (Get-Process -Id $PID).Path
    & $shell -NoProfile -ExecutionPolicy Bypass -File $detect -Version $Version
    if ($LASTEXITCODE -ne 0) { throw 'Device installation is incomplete; inspect setup and the managed scheduled task.' }
    exit 0
} catch {
    Write-Error $_
    exit 1
}
