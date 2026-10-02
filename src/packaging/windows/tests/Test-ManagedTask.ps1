# Prove a group task preserves the standard user's identity, profile, session, and ordinary token.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'InteractiveSession.ps1')
$sessionId = (Get-Process -Id $PID).SessionId
if ($sessionId -le 0) { throw 'This test requires a logged-in Windows session, not session zero.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this test from an elevated Windows shell.'
}
$temp = Join-Path $env:ProgramData ('quesma-task-test-' + [guid]::NewGuid())
$taskName = 'Quesma Shipper Task Test - ' + [guid]::NewGuid()
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$task = $null
$testUser = $null
$testSession = $null
$terminalServerKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Terminal Server'
$previousDeny = (Get-ItemProperty -LiteralPath $terminalServerKey).fDenyTSConnections
$firewallRule = 'quesma-rdp-test-' + [guid]::NewGuid()
try {
    New-Item -ItemType Directory -Path $temp | Out-Null
    $name = 'quesmatest' + [guid]::NewGuid().ToString('N').Substring(0, 10)
    $plainPassword = [guid]::NewGuid().ToString('N') + 'aA1!'
    $password = ConvertTo-SecureString $plainPassword -AsPlainText -Force
    $testUser = New-LocalUser -Name $name -Password $password -AccountNeverExpires -PasswordNeverExpires
    $users = Get-LocalGroup -SID 'S-1-5-32-545'
    if (-not (Get-LocalGroupMember -Group $users | Where-Object { $_.SID -eq $testUser.SID })) {
        Add-LocalGroupMember -Group $users -Member $testUser
    }
    Add-LocalGroupMember -Group (Get-LocalGroup -SID 'S-1-5-32-555') -Member $testUser
    $acl = Get-Acl -LiteralPath $temp
    $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
        $testUser.SID, 'Modify', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
    Set-Acl -LiteralPath $temp -AclObject $acl
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
} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $PSScriptRoot "$PID.tmp")
Move-Item -LiteralPath (Join-Path $PSScriptRoot "$PID.tmp") -Destination (Join-Path $PSScriptRoot "$PID.json")
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
    $task = $folder.RegisterTaskDefinition($taskName, $definition, 22, $null, $null, 4,
        'D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;IU)')
    Set-ItemProperty -LiteralPath $terminalServerKey -Name fDenyTSConnections -Value 0
    New-NetFirewallRule -Name $firewallRule -DisplayName $firewallRule -Direction Inbound -Action Allow `
        -Protocol TCP -LocalPort 3389 -LocalAddress 127.0.0.1 -RemoteAddress 127.0.0.1 | Out-Null
    Start-Service TermService
    $testSession = Open-TestSession $name $plainPassword
    $deadline = (Get-Date).AddSeconds(30)
    do {
        [Windows.Forms.Application]::DoEvents()
        $reports = @(Get-ChildItem -LiteralPath $temp -Filter '*.json')
        if ($reports.Count -eq 1) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    if ($reports.Count -ne 1) { throw 'The group logon trigger did not start exactly one standard-user child.' }
    $profile = Get-ItemProperty -LiteralPath ("HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\" + $testUser.SID.Value)
    $expectedProfile = [Environment]::ExpandEnvironmentVariables($profile.ProfileImagePath)
    $null = $task.RunEx($null, 4, $testSession.Id, $null)
    $deadline = (Get-Date).AddSeconds(30)
    do {
        [Windows.Forms.Application]::DoEvents()
        $reports = @(Get-ChildItem -LiteralPath $temp -Filter '*.json')
        if ($reports.Count -eq 2) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    if ($reports.Count -ne 2) {
        throw "Expected two parallel task children, got $($reports.Count); task result=$($task.LastTaskResult)"
    }
    foreach ($reportFile in $reports) {
        $report = Get-Content -LiteralPath $reportFile.FullName -Raw | ConvertFrom-Json
        if ($report.SID -ne $testUser.SID.Value -or $report.SID -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20') -or
            $report.Profile -ne $expectedProfile -or $report.Session -ne $testSession.Id -or $report.Elevated) {
            throw "Task did not use the unelevated interactive user: $($report | ConvertTo-Json -Compress)"
        }
    }
    Write-Output 'Passed: real standard-user logon trigger, administrator session-targeted RunEx, parallel children, user SID/profile/session, and least privilege.'
} finally {
    if ($task) {
        $task.Stop(0)
        $folder.DeleteTask($taskName, 0)
    }
    Close-TestSession $testSession
    Set-ItemProperty -LiteralPath $terminalServerKey -Name fDenyTSConnections -Value $previousDeny
    Remove-NetFirewallRule -Name $firewallRule -ErrorAction SilentlyContinue
    if ($testUser) {
        Remove-LocalUser -SID $testUser.SID
        Get-CimInstance Win32_UserProfile -Filter "SID='$($testUser.SID.Value)'" |
            Remove-CimInstance -ErrorAction Continue
    }
    Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
