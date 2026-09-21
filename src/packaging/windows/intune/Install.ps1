# Configure a private copy, outside the repository, before uploading to Intune.
# The same script accepts a bundled installer for Win32 app deployments.
[CmdletBinding()]
param(
    [string]$Server = 'https://cp.example.com',
    [string]$Organization = 'Example Organization',
    [string]$Grant = 'REPLACE_WITH_ENROLLMENT_GRANT',
    [string]$InstallerUrl = 'https://downloads.example.com/QuesmaShipperSetup-amd64.exe',
    [string]$InstallerPath = ''
)

$ErrorActionPreference = 'Stop'

function Get-ShipperStatus([string]$Executable, [switch]$AllowLaunchFailure) {
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo.FileName = $Executable
    $process.StartInfo.Arguments = 'status --json'
    $process.StartInfo.UseShellExecute = $false
    $process.StartInfo.CreateNoWindow = $true
    $process.StartInfo.RedirectStandardOutput = $true
    $process.StartInfo.StandardOutputEncoding = [System.Text.Encoding]::UTF8
    try {
        # Only an OS launch failure permits repair; a running shipper's errors must propagate.
        try { $null = $process.Start() }
        catch [System.ComponentModel.Win32Exception] {
            if ($AllowLaunchFailure) { return $null }
            throw
        }
        $raw = $process.StandardOutput.ReadToEnd()
        $process.WaitForExit()
        if ($process.ExitCode -ne 0) { throw 'Could not read shipper status; inspect configuration and enrollment.' }
        $status = $raw | ConvertFrom-Json
        if ($null -eq $status -or $status.logged_in -isnot [bool] -or $status.service_ok -isnot [bool]) {
            throw 'Shipper returned an invalid status response.'
        }
        return $status
    } finally { $process.Dispose() }
}

function Assert-EnrollmentTarget($Status, [string]$ExpectedServer, [string]$ExpectedOrganization) {
    if ($Status.endpoint -and (
        $Status.endpoint.TrimEnd('/') -cne $ExpectedServer.TrimEnd('/') -or
        $Status.organization -cne $ExpectedOrganization)) {
        throw 'Existing enrollment belongs to a different server or organization; resolve it manually.'
    }
}

function Install-Shipper {
    $downloadDir = $null
    try {
        if ($Server -eq 'https://cp.example.com' -or ([uri]$Server).Scheme -ne 'https' -or
            [string]::IsNullOrWhiteSpace($Organization) -or $Organization -eq 'Example Organization') {
            throw 'Set the HTTPS Server and exact Organization name in your private script copy.'
        }

        $exe = Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper\quesma-shipper.exe'
        $supervisor = Join-Path (Split-Path $exe) 'quesma-shipper-supervisor.exe'
        $status = $null
        if (Test-Path -LiteralPath $exe -PathType Leaf) {
            $status = Get-ShipperStatus $exe -AllowLaunchFailure
            Assert-EnrollmentTarget $status $Server $Organization
        }

        # Reusing the installed copy preserves self-updates when enrollment is retried.
        if (-not $status -or -not $status.service_ok -or -not (Test-Path -LiteralPath $supervisor -PathType Leaf)) {
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
            $setup = Start-Process -FilePath (Resolve-Path -LiteralPath $InstallerPath).Path -Wait -PassThru `
                -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-'
            if ($setup.ExitCode -ne 0) { throw "Installer failed with exit code $($setup.ExitCode)." }
            $status = Get-ShipperStatus $exe
            Assert-EnrollmentTarget $status $Server $Organization
        }

        if (-not $status.endpoint) {
            if ([string]::IsNullOrWhiteSpace($Grant) -or $Grant -eq 'REPLACE_WITH_ENROLLMENT_GRANT') {
                throw 'Set Grant to an enrollment grant in your private script copy.'
            }
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
    } finally {
        if ($downloadDir) { Remove-Item -LiteralPath $downloadDir -Recurse -Force -ErrorAction SilentlyContinue }
    }
}

try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'Run this script on Windows through Intune; it can be prepared on macOS.'
    }
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    if ($sid -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20')) {
        throw 'Use logged-on user credentials / User install behavior, not a service account.'
    }
    Install-Shipper
    exit 0
} catch {
    Write-Error $_
    exit 1
}
