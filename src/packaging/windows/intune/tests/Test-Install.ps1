# Invoked by Test-Deployment.ps1 with the real installer functions and a synthetic executable.
param([string]$Temp, [string]$Template, [hashtable]$Good)
$ErrorActionPreference = 'Stop'
$Server = 'https://control.example.com'
$Organization = 'Test Organization'
$InstallerPath = $Template
$InstallerUrl = ''
$exe = Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper\quesma-shipper.exe'
$supervisor = Join-Path (Split-Path $exe) 'quesma-shipper-supervisor.exe'
$env:QUESMA_INTUNE_TEST_LOGIN = Join-Path $Temp 'login.json'
$env:QUESMA_INTUNE_TEST_ENROLLED = $Good | ConvertTo-Json -Compress
$savedToken = $env:SHIPPER_AUTH_KEY
$state = Join-Path $Temp 'upload-state'
Set-Content -LiteralPath $state -Value 'preserve-existing-state'

function Start-Process {
    param($FilePath, [switch]$Wait, [switch]$PassThru, $ArgumentList)
    if ($FilePath -ne $Template -or -not $Wait -or -not $PassThru -or
        $ArgumentList -ne '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-') {
        throw 'Unexpected setup invocation.'
    }
    if ($env:SHIPPER_AUTH_KEY -ne 'existing-token') { throw 'Setup inherited the enrollment grant.' }
    $script:setupCount++
    Copy-Item $Template $exe -Force
    Set-Content -LiteralPath $supervisor -Value 'synthetic supervisor'
    $env:QUESMA_INTUNE_TEST_STATUS = $script:afterSetup | ConvertTo-Json -Compress
    $env:QUESMA_INTUNE_TEST_EXIT = [string]$script:afterExit
    return [pscustomobject]@{ ExitCode = 0 }
}

$cases = @(
    @{ Name = 'healthy enrolled copy'; Setups = 0 },
    @{ Name = 'missing executable'; Damage = 'missing'; Setups = 1 },
    @{ Name = 'unlaunchable executable'; Damage = 'invalid'; Setups = 1 },
    @{ Name = 'missing supervisor'; Damage = 'supervisor'; Setups = 1 },
    @{ Name = 'missing task'; Patch = @{ service = 'not installed'; service_ok = $false }; Setups = 1 },
    @{ Name = 'disabled task'; Patch = @{ service = 'installed but not running'; service_ok = $false }; Setups = 1 },
    @{ Name = 'status exits unsuccessfully'; Exit = 2; Setups = 0; Error = 'Could not read shipper status' },
    @{ Name = 'malformed JSON'; Raw = 'invalid'; Setups = 0; Error = '*' },
    @{ Name = 'empty status'; Raw = ''; Setups = 0; Error = 'invalid status response' },
    @{ Name = 'incomplete status'; Raw = '{}'; Setups = 0; Error = 'invalid status response' },
    @{ Name = 'wrong server'; Patch = @{ endpoint = 'https://other.example.com' }; Setups = 0; Error = 'different server or organization' },
    @{ Name = 'wrong organization'; Patch = @{ organization = 'Other Organization' }; Setups = 0; Error = 'different server or organization' },
    @{ Name = 'wrong server after repair'; Damage = 'invalid'; After = @{ endpoint = 'https://other.example.com' }; Setups = 1; Error = 'different server or organization' },
    @{ Name = 'wrong organization after repair'; Damage = 'missing'; After = @{ organization = 'Other Organization' }; Setups = 1; Error = 'different server or organization' },
    @{ Name = 'status failure after repair'; Damage = 'invalid'; AfterExit = 2; Setups = 1; Error = 'Could not read shipper status' },
    @{ Name = 'missing identity'; Patch = @{ logged_in = $false }; Setups = 0; Error = 'incomplete' },
    @{ Name = 'unenrolled without grant'; Patch = @{ logged_in = $false; endpoint = ''; organization = '' }; Setups = 0; Error = 'Set Grant' },
    @{ Name = 'fresh install without grant'; Damage = 'missing'; After = @{ logged_in = $false; endpoint = ''; organization = '' }; Setups = 1; Error = 'Set Grant' },
    @{ Name = 'fresh install with grant'; Damage = 'missing'; After = @{ logged_in = $false; endpoint = ''; organization = '' }; Grant = 'synthetic-grant'; Setups = 1; Login = $true }
)

try {
    foreach ($case in $cases) {
        $Grant = if ($case.Grant) { $case.Grant } else { '' }
        $env:SHIPPER_AUTH_KEY = 'existing-token'
        Remove-Item -LiteralPath $env:QUESMA_INTUNE_TEST_LOGIN -ErrorAction SilentlyContinue
        Copy-Item $Template $exe -Force
        Set-Content -LiteralPath $supervisor -Value 'synthetic supervisor'
        $status = $Good.Clone()
        if ($case.Patch) { foreach ($key in $case.Patch.Keys) { $status[$key] = $case.Patch[$key] } }
        $script:afterSetup = $status.Clone()
        $script:afterSetup.service = 'windows-task'
        $script:afterSetup.service_ok = $true
        if ($case.After) { foreach ($key in $case.After.Keys) { $script:afterSetup[$key] = $case.After[$key] } }
        $script:afterExit = if ($case.AfterExit) { $case.AfterExit } else { 0 }
        $env:QUESMA_INTUNE_TEST_STATUS = $status | ConvertTo-Json -Compress
        if ($case.ContainsKey('Raw')) { $env:QUESMA_INTUNE_TEST_STATUS = $case.Raw }
        $env:QUESMA_INTUNE_TEST_EXIT = if ($case.Exit) { [string]$case.Exit } else { '0' }
        switch ($case.Damage) {
            'missing' { Remove-Item -LiteralPath $exe }
            'invalid' { [IO.File]::WriteAllText($exe, 'sss') }
            'supervisor' { Remove-Item -LiteralPath $supervisor }
        }
        $script:setupCount = 0
        $failure = $null
        try { Install-Shipper | Out-Null } catch { $failure = $_ }
        if ([bool]$failure -ne [bool]$case.Error -or
            ($failure -and $failure.ToString() -notlike "*$($case.Error)*") -or
            $script:setupCount -ne $case.Setups) {
            throw "Installation failed: $($case.Name), setups=$script:setupCount, error=$failure"
        }
        if ((Test-Path -LiteralPath $env:QUESMA_INTUNE_TEST_LOGIN) -ne [bool]$case.Login) {
            throw "Unexpected enrollment attempt: $($case.Name)"
        }
        if ($env:SHIPPER_AUTH_KEY -ne 'existing-token' -or
            (Get-Content -LiteralPath $state) -ne 'preserve-existing-state') {
            throw "Token or upload state changed: $($case.Name)"
        }
    }
    Write-Output "Passed: $($cases.Count) installation and repair scenarios."
} finally {
    $env:SHIPPER_AUTH_KEY = $savedToken
}
