# Builds one architecture-specific per-user setup executable with the Inno compiler on the runner.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ReleaseVersion,
    [Parameter(Mandatory = $true)][ValidateSet("amd64", "arm64")][string]$Architecture,
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$OutputDir
)

$ErrorActionPreference = "Stop"
$Here = $PSScriptRoot
$BinaryPath = (Resolve-Path $BinaryPath).Path
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path

if ($Architecture -eq "amd64" -or $env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $banner = & $BinaryPath --version
    if ($LASTEXITCODE -ne 0 -or $banner -notlike "quesma-shipper $ReleaseVersion (*") {
        throw "binary did not corroborate release version $ReleaseVersion`: $banner"
    }
}

$compiler = Get-Command ISCC.exe -ErrorAction SilentlyContinue
if (-not $compiler) {
    $candidates = @(
        "${env:ProgramFiles(x86)}\Inno Setup 7\ISCC.exe",
        "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
        "$env:ProgramFiles\Inno Setup 7\ISCC.exe",
        "$env:ProgramFiles\Inno Setup 6\ISCC.exe"
    )
    $compiler = $candidates | Where-Object { Test-Path $_ } | Select-Object -First 1
}
if (-not $compiler) {
    throw "Inno Setup compiler (ISCC.exe) is required"
}

& $compiler "/DReleaseVersion=$ReleaseVersion" "/DArchitecture=$Architecture" `
    "/DBinaryPath=$BinaryPath" "/DOutputDir=$OutputDir" "$Here\setup.iss"
if ($LASTEXITCODE -ne 0) {
    throw "Inno Setup failed with exit code $LASTEXITCODE"
}

$setup = Join-Path $OutputDir "QuesmaShipperSetup-$Architecture.exe"
if (-not (Test-Path $setup)) {
    throw "Inno Setup did not create $setup"
}
Write-Output "built $setup"
