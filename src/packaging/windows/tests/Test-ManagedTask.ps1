# Prove the group principal and RunEx session semantics on Windows before relying on them in setup.
$ErrorActionPreference = 'Stop'
$sessionId = (Get-Process -Id $PID).SessionId
if ($sessionId -le 0) { throw 'This test requires a logged-in Windows session, not session zero.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this test from an elevated Windows shell.'
}
$temp = Join-Path ([IO.Path]::GetTempPath()) ('quesma-task-test-' + [guid]::NewGuid())
$taskName = 'Quesma Shipper Task Test - ' + [guid]::NewGuid()
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$task = $null
try {
    New-Item -ItemType Directory -Path $temp | Out-Null
    $childScript = Join-Path $temp 'identity.ps1'
    @'
$ErrorActionPreference = 'Stop'
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
@{
    SID = $identity.User.Value
    Profile = $env:USERPROFILE
    Session = (Get-Process -Id $PID).SessionId
    Elevated = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $PSScriptRoot "$PID.json")
Start-Sleep -Seconds 90
'@ | Set-Content -LiteralPath $childScript -Encoding UTF8

    $definition = $scheduler.NewTask(0)
    $definition.Principal.GroupId = 'S-1-5-32-545'
    $definition.Principal.RunLevel = 0
    $definition.Triggers.Create(9).Enabled = $true
    $definition.Settings.MultipleInstances = 0
    $definition.Settings.ExecutionTimeLimit = 'PT2M'
    $definition.Settings.DisallowStartIfOnBatteries = $false
    $definition.Settings.StopIfGoingOnBatteries = $false
    $action = $definition.Actions.Create(0)
    $action.Path = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
    $action.Arguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $childScript + '"'
    $task = $folder.RegisterTaskDefinition($taskName, $definition, 6, $null, $null, 4,
        'D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;IU)')
    $null = $task.RunEx($null, 4, $sessionId, $null)
    $null = $task.RunEx($null, 4, $sessionId, $null)
    $deadline = (Get-Date).AddSeconds(30)
    do {
        $reports = @(Get-ChildItem -LiteralPath $temp -Filter '*.json')
        if ($reports.Count -eq 2) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    if ($reports.Count -ne 2) {
        throw "Expected two parallel task children, got $($reports.Count); task result=$($task.LastTaskResult)"
    }
    foreach ($reportFile in $reports) {
        $report = Get-Content -LiteralPath $reportFile.FullName -Raw | ConvertFrom-Json
        if ($report.SID -ne $identity.User.Value -or $report.SID -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20') -or
            $report.Profile -ne $env:USERPROFILE -or $report.Session -ne $sessionId -or $report.Elevated) {
            throw "Task did not use the unelevated interactive user: $($report | ConvertTo-Json -Compress)"
        }
    }
    Write-Output 'Passed: interactive group task, session-targeted RunEx, parallel children, user SID/profile, and least privilege.'
} finally {
    if ($task) {
        $task.Stop(0)
        $folder.DeleteTask($taskName, 0)
    }
    Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
