# Builds one architecture-specific per-user setup executable with the Inno compiler on the runner.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ReleaseVersion,
    [Parameter(Mandatory = $true)][ValidateSet("amd64", "arm64")][string]$Architecture,
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$SupervisorPath,
    [Parameter(Mandatory = $true)][string]$OutputDir
)

$ErrorActionPreference = "Stop"
$Here = $PSScriptRoot
$BinaryPath = (Resolve-Path $BinaryPath).Path
$SupervisorPath = (Resolve-Path $SupervisorPath).Path
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

$compilerPath = if ($compiler -is [System.Management.Automation.CommandInfo]) {
    $compiler.Source
} else {
    $compiler
}
$compilerVersion = (Get-Item $compilerPath).VersionInfo.ProductVersion
if ($compilerVersion -notmatch '^(\d+)\.(\d+)') {
    throw "could not determine the Inno Setup version from '$compilerVersion'"
}
if ([version]::new([int]$Matches[1], [int]$Matches[2]) -lt [version]'6.3') {
    throw "Inno Setup 6.3 or newer is required; found $compilerVersion"
}

if ($ReleaseVersion -notmatch '^(\d+)\.(\d+)\.(\d+)-(\d+)\.[0-9A-Za-z]+$') {
    throw "release version must have the form major.minor.patch-commit.sha; got $ReleaseVersion"
}
$fileVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3]).$($Matches[4])"

& $compilerPath "/DReleaseVersion=$ReleaseVersion" "/DFileVersion=$fileVersion" `
    "/DArchitecture=$Architecture" "/DBinaryPath=$BinaryPath" `
    "/DSupervisorPath=$SupervisorPath" "/DOutputDir=$OutputDir" "$Here\setup.iss"
if ($LASTEXITCODE -ne 0) {
    throw "Inno Setup failed with exit code $LASTEXITCODE"
}

$setup = Join-Path $OutputDir "QuesmaShipperSetup-$Architecture.exe"
if (-not (Test-Path $setup)) {
    throw "Inno Setup did not create $setup"
}
Write-Output "built $setup"
