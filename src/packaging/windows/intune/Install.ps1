# Configure a private copy, outside the repository, before uploading to Intune.
# The same script accepts a bundled installer for Win32 app deployments.
[CmdletBinding()]
param(
    [string]$Server = 'https://cp.example.com',
    [string]$Organization = 'Example Organization',
    [string]$Grant = 'REPLACE_WITH_ENROLLMENT_GRANT',
    [string]$InstallerUrl = 'https://downloads.example.com/QuesmaShipperSetup-amd64.exe',
    [string]$InstallerSha256 = 'REPLACE_WITH_INSTALLER_SHA256',
    [string]$InstallerPath = ''
)

$ErrorActionPreference = 'Stop'

function Get-ShipperStatus([string]$Executable) {
    $raw = & $Executable status --json
    if ($LASTEXITCODE -ne 0) { throw 'Could not read shipper status.' }
    return ($raw -join "`n") | ConvertFrom-Json
}

function Assert-EnrollmentTarget($Status, [string]$ExpectedServer, [string]$ExpectedOrganization) {
    if ($Status.endpoint -and (
        $Status.endpoint.TrimEnd('/') -cne $ExpectedServer.TrimEnd('/') -or
        $Status.organization -cne $ExpectedOrganization)) {
        throw 'Existing enrollment belongs to a different server or organization; resolve it manually.'
    }
}

$downloadDir = $null
try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'Run this script on Windows through Intune; it can be prepared on macOS.'
    }
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    if ($sid -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20')) {
        throw 'Use logged-on user credentials / User install behavior, not a service account.'
    }
    if ($Server -eq 'https://cp.example.com' -or ([uri]$Server).Scheme -ne 'https' -or
        [string]::IsNullOrWhiteSpace($Organization) -or $Organization -eq 'Example Organization') {
        throw 'Set the HTTPS Server and exact Organization name in your private script copy.'
    }

    $exe = Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper\quesma-shipper.exe'
    $supervisor = Join-Path (Split-Path $exe) 'quesma-shipper-supervisor.exe'
    $status = $null
    if (Test-Path -LiteralPath $exe -PathType Leaf) {
        $status = Get-ShipperStatus $exe
        Assert-EnrollmentTarget $status $Server $Organization
    }
    if (-not $status.endpoint -and (
        [string]::IsNullOrWhiteSpace($Grant) -or $Grant -eq 'REPLACE_WITH_ENROLLMENT_GRANT')) {
        throw 'Set Grant to an enrollment grant in your private script copy.'
    }

    # Reusing the installed copy preserves self-updates when enrollment is retried.
    if (-not $status -or -not $status.service_ok -or -not (Test-Path -LiteralPath $supervisor -PathType Leaf)) {
        if ($InstallerSha256 -notmatch '^[0-9a-fA-F]{64}$') {
            throw 'Set InstallerSha256 to the SHA-256 of the approved setup executable.'
        }
        if (-not $InstallerPath) {
            if (([uri]$InstallerUrl).Scheme -ne 'https' -or ([uri]$InstallerUrl).Host -eq 'downloads.example.com') {
                throw 'Set InstallerUrl to an HTTPS download of the approved setup executable.'
            }
            $downloadDir = Join-Path ([IO.Path]::GetTempPath()) ('quesma-intune-' + [guid]::NewGuid())
            New-Item -ItemType Directory -Path $downloadDir | Out-Null
            $InstallerPath = Join-Path $downloadDir 'setup.exe'
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -Uri $InstallerUrl -OutFile $InstallerPath -UseBasicParsing -TimeoutSec 300
        }
        if ((Get-FileHash -LiteralPath $InstallerPath -Algorithm SHA256).Hash -ne $InstallerSha256) {
            throw 'Installer SHA-256 mismatch; no installer was executed.'
        }
        $setup = Start-Process -FilePath (Resolve-Path -LiteralPath $InstallerPath).Path -Wait -PassThru `
            -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-'
        if ($setup.ExitCode -ne 0) { throw "Installer failed with exit code $($setup.ExitCode)." }
        $status = Get-ShipperStatus $exe
        Assert-EnrollmentTarget $status $Server $Organization
    }

    if (-not $status.endpoint) {
        $previousToken = $env:SHIPPER_AUTH_KEY
        $loginFailed = $false
        try {
            # Scope the grant to login; the installer and background task must not inherit it.
            $env:SHIPPER_AUTH_KEY = $Grant
            & $exe login --server $Server 2>&1 | Out-Null
            $loginFailed = $LASTEXITCODE -ne 0
        } catch {
            $loginFailed = $true
        } finally {
            $env:SHIPPER_AUTH_KEY = $previousToken
        }
        if ($loginFailed) { throw 'Login failed; check grant validity and control-plane connectivity.' }
    }
    $status = Get-ShipperStatus $exe
    Assert-EnrollmentTarget $status $Server $Organization
    if (-not $status.logged_in -or -not $status.endpoint -or -not $status.service_ok) {
        throw 'Installation or enrollment is incomplete; inspect shipper status on the device.'
    }
    Write-Output 'Quesma Shipper is installed, enrolled, and enabled for this user.'
    exit 0
} catch {
    Write-Error $_
    exit 1
} finally {
    if ($downloadDir) { Remove-Item -LiteralPath $downloadDir -Recurse -Force -ErrorAction SilentlyContinue }
}
