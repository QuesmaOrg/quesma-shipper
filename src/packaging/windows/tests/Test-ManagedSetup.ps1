# Exercise the real installer as SYSTEM while validating the collector in the logged-in user's session.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InstallerPath,
    [Parameter(Mandatory = $true)][string]$InitialVersion,
    [Parameter(Mandatory = $true)][string]$UpgradeInstallerPath,
    [Parameter(Mandatory = $true)][string]$ExpectedVersion
)
$ErrorActionPreference = 'Stop'
$InstallerPath = (Resolve-Path $InstallerPath).Path
$UpgradeInstallerPath = (Resolve-Path $UpgradeInstallerPath).Path
$temp = Join-Path ([IO.Path]::GetTempPath()) ('quesma-managed-test-' + [guid]::NewGuid())
$installDir = Join-Path $env:ProgramFiles 'Quesma Shipper'
$shipperPath = Join-Path $installDir 'quesma-shipper.exe'
$supervisorPath = Join-Path $installDir 'quesma-shipper-supervisor.exe'
$taskName = 'Quesma Shipper Managed'
$sid = ([Security.Principal.WindowsIdentity]::GetCurrent()).User.Value
$sessionId = (Get-Process -Id $PID).SessionId
if ($sessionId -le 0) { throw 'This test requires an interactive Windows session.' }
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$stateDir = Join-Path $env:USERPROFILE '.local\state\trajectory-shipper'
$stateMarker = Join-Path $stateDir ('setup-test-' + [guid]::NewGuid())
$nativeRegistry = [Microsoft.Win32.RegistryKey]::OpenBaseKey(
    [Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryView]::Registry64)
$detectionScript = (Resolve-Path (Join-Path $PSScriptRoot '..\intune\Detect-System.ps1')).Path

function Invoke-SystemProcess([string]$FilePath, [string]$Arguments) {
    $id = [guid]::NewGuid().ToString()
    $scriptPath = Join-Path $temp "$id.ps1"
    $resultPath = Join-Path $temp "$id.json"
    $payload = @{ FilePath = $FilePath; Arguments = $Arguments; ResultPath = $resultPath } | ConvertTo-Json -Compress
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($payload))
    $script = @'
$ErrorActionPreference = 'Stop'
$payload = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('PAYLOAD')) | ConvertFrom-Json
try {
    $process = Start-Process -FilePath $payload.FilePath -ArgumentList $payload.Arguments -Wait -PassThru `
        -RedirectStandardOutput ($payload.ResultPath + '.stdout') -RedirectStandardError ($payload.ResultPath + '.stderr')
    @{ ExitCode = $process.ExitCode; SID = ([Security.Principal.WindowsIdentity]::GetCurrent()).User.Value
        Output = Get-Content -LiteralPath ($payload.ResultPath + '.stdout') -Raw
        ErrorOutput = Get-Content -LiteralPath ($payload.ResultPath + '.stderr') -Raw } |
        ConvertTo-Json | Set-Content -LiteralPath ($payload.ResultPath + '.tmp')
    Move-Item -LiteralPath ($payload.ResultPath + '.tmp') -Destination $payload.ResultPath
} catch {
    @{ Error = $_.ToString() } | ConvertTo-Json | Set-Content -LiteralPath ($payload.ResultPath + '.tmp')
    Move-Item -LiteralPath ($payload.ResultPath + '.tmp') -Destination $payload.ResultPath
    exit 1
}
'@
    $script.Replace('PAYLOAD', $encoded) | Set-Content -LiteralPath $scriptPath -Encoding UTF8
    $systemTask = 'Quesma Shipper SYSTEM Test - ' + $id
    $registered = $null
    try {
        $definition = $scheduler.NewTask(0)
        $definition.Principal.UserId = 'S-1-5-18'
        $definition.Principal.LogonType = 5
        $definition.Principal.RunLevel = 1
        $definition.Settings.ExecutionTimeLimit = 'PT3M'
        $action = $definition.Actions.Create(0)
        $action.Path = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
        $action.Arguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $scriptPath + '"'
        $registered = $folder.RegisterTaskDefinition($systemTask, $definition, 6, 'S-1-5-18', $null, 5)
        $null = $registered.Run($null)
        $deadline = (Get-Date).AddSeconds(150)
        while (-not (Test-Path -LiteralPath $resultPath)) {
            if ((Get-Date) -ge $deadline) { throw "SYSTEM command timed out: $FilePath" }
            Start-Sleep -Milliseconds 250
        }
        $result = Get-Content -LiteralPath $resultPath -Raw | ConvertFrom-Json
        if ($result.Error -or $result.SID -ne 'S-1-5-18') {
            throw "Command did not complete as SYSTEM: $($result | ConvertTo-Json -Compress)"
        }
        if ($result.ExitCode -ne 0) {
            if ($result.Output) { Write-Host $result.Output }
            if ($result.ErrorOutput) { Write-Host $result.ErrorOutput }
        }
        return $result.ExitCode
    } finally {
        if ($registered) {
            $registered.Stop(0)
            $folder.DeleteTask($systemTask, 0)
        }
    }
}

function Invoke-Setup([string]$Path) {
    $log = Join-Path $temp ('setup-' + [guid]::NewGuid() + '.log')
    $code = Invoke-SystemProcess $Path ('/ALLUSERS /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /LOG="' + $log + '"')
    if ($code -ne 0) {
        if (Test-Path -LiteralPath $log) { Get-Content -LiteralPath $log | Write-Host }
        throw "Machine installer exited $code"
    }
}

function Get-ManagedProcesses {
    @(Get-CimInstance Win32_Process -Filter "Name = 'quesma-shipper.exe' OR Name = 'quesma-shipper-supervisor.exe'" |
        Where-Object { $_.ExecutablePath -in @($shipperPath, $supervisorPath) })
}

function Assert-Detected([string]$Version, [bool]$Expected = $true) {
    $arguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $detectionScript +
        '" -Version "' + $Version + '"'
    $code = Invoke-SystemProcess "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" $arguments
    $expectedCode = if ($Expected) { 0 } else { 1 }
    if ($code -ne $expectedCode) { throw "SYSTEM detection returned $code, expected $expectedCode" }
}

function Wait-Stopped {
    $deadline = (Get-Date).AddSeconds(15)
    while (@(Get-ManagedProcesses).Count -gt 0 -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }
    if (@(Get-ManagedProcesses).Count) { throw 'Managed processes did not stop.' }
}

function Assert-Live {
    $live = $false
    $deadline = (Get-Date).AddSeconds(30)
    do {
        $processes = @(Get-ManagedProcesses)
        $ownProcesses = @($processes | Where-Object { $_.SessionId -eq $sessionId })
        if (@($ownProcesses | Where-Object Name -eq 'quesma-shipper.exe').Count -eq 1 -and
            @($ownProcesses | Where-Object Name -eq 'quesma-shipper-supervisor.exe').Count -eq 1) { $live = $true; break }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    if (-not $live) {
        throw "Expected one supervisor and one collector in session $sessionId; found $($ownProcesses.Count)"
    }
    foreach ($process in $processes) {
        $owner = Invoke-CimMethod -InputObject $process -MethodName GetOwnerSid
        if ($owner.ReturnValue -ne 0 -or $owner.Sid -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20') -or
            ($process.SessionId -eq $sessionId -and $owner.Sid -ne $sid)) {
            throw "Managed process $($process.ProcessId) ran with unexpected identity $($owner.Sid)"
        }
    }
}

function Assert-Installed([string]$Version) {
    foreach ($path in @($shipperPath, $supervisorPath, (Join-Path $installDir 'unins000.exe'))) {
        if (-not (Test-Path -LiteralPath $path)) { throw "Missing managed file: $path" }
    }
    $key = $nativeRegistry.OpenSubKey('Software\Quesma\Shipper')
    if (-not $key) { throw 'Native machine registration is missing.' }
    try {
        if ($key.GetValue('InstallDir') -ne $installDir -or $key.GetValue('Version') -ne $Version) {
            throw 'Machine registration has the wrong directory or version.'
        }
    } finally { $key.Dispose() }
    $task = $folder.GetTask($taskName)
    [xml]$xml = $task.Xml
    $group = $xml.Task.Principals.Principal.GroupId
    if ($group -notlike 'S-*') {
        $group = (New-Object Security.Principal.NTAccount($group)).Translate([Security.Principal.SecurityIdentifier]).Value
    }
    if (-not $task.Enabled -or $group -ne 'S-1-5-32-545' -or
        $task.Definition.Principal.RunLevel -ne 0 -or
        $xml.Task.Settings.MultipleInstancesPolicy -ne 'Parallel' -or
        -not $xml.Task.Triggers.LogonTrigger -or $xml.Task.Triggers.LogonTrigger.UserId -or
        $xml.Task.Actions.Exec.Command -ne $supervisorPath -or $xml.Task.Actions.Exec.Arguments -ne '--managed') {
        throw "Unexpected managed task definition: $($task.Xml)"
    }
    $taskSecurity = New-Object Security.AccessControl.RawSecurityDescriptor($task.GetSecurityDescriptor(4))
    foreach ($ace in $taskSecurity.DiscretionaryAcl) {
        if ($ace.AceQualifier -eq 'AccessAllowed' -and
            $ace.SecurityIdentifier.Value -in @('S-1-1-0', 'S-1-5-4', 'S-1-5-11', 'S-1-5-32-545') -and
            ([int64]$ace.AccessMask -band 0x700D0176) -ne 0) {
            throw 'Ordinary users can execute, stop, or modify the managed scheduled task.'
        }
    }
    foreach ($path in @($installDir, $shipperPath, $supervisorPath)) {
        foreach ($rule in (Get-Acl -LiteralPath $path).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
            $ruleSID = $rule.IdentityReference.Value
            if ($rule.AccessControlType -eq 'Allow' -and $ruleSID -in @('S-1-1-0', 'S-1-5-4', 'S-1-5-11', 'S-1-5-32-545') -and
                ([int]$rule.FileSystemRights -band 0xD0156) -ne 0) {
                throw "Ordinary users can modify managed program files: $path ($ruleSID)"
            }
        }
    }
    if ((Get-Content -LiteralPath $stateMarker -Raw).Trim() -ne 'preserve-user-state') {
        throw 'The installer changed existing user state.'
    }
    Assert-Live
    Assert-Detected $Version
}

function Get-SystemState {
    $paths = @(
        "$env:SystemRoot\System32\config\systemprofile\.local\state\trajectory-shipper",
        "$env:SystemRoot\SysWOW64\config\systemprofile\.local\state\trajectory-shipper",
        "$env:SystemRoot\ServiceProfiles\LocalService\.local\state\trajectory-shipper",
        "$env:SystemRoot\ServiceProfiles\NetworkService\.local\state\trajectory-shipper"
    )
    @($paths | Where-Object { Test-Path -LiteralPath $_ } | ForEach-Object {
        Get-ChildItem -LiteralPath $_ -File -Recurse | Select-Object FullName, Length, LastWriteTimeUtc
    }) | ConvertTo-Json -Compress
}

try {
    New-Item -ItemType Directory -Path $temp, $stateDir -Force | Out-Null
    Set-Content -LiteralPath $stateMarker -Value 'preserve-user-state'
    $systemState = Get-SystemState
    Invoke-Setup $InstallerPath
    Assert-Installed $InitialVersion

    $recovery = Join-Path $installDir '.test-recovery.json'
    $recoveryArguments = '--install-dir "' + $installDir + '" --recovery-file "' + $recovery + '"'
    foreach ($enabledBefore in @($true, $false)) {
        $task = $folder.GetTask($taskName)
        if (-not $enabledBefore) { $task.Enabled = $false; $task.Stop(0); Wait-Stopped }
        foreach ($attempt in 1..2) {
            $code = Invoke-SystemProcess $shipperPath ('prepare-system-install ' + $recoveryArguments)
            if ($code -ne 0) { throw "Upgrade preparation failed: $code" }
        }
        $task = $folder.GetTask($taskName)
        if ($task.Enabled) { throw 'Upgrade preparation did not disable new task launches.' }
        Wait-Stopped
        $code = Invoke-SystemProcess $shipperPath ('resume-system-install ' + $recoveryArguments)
        if ($code -ne 0 -or (Test-Path -LiteralPath $recovery)) { throw 'Upgrade recovery failed.' }
        $task = $folder.GetTask($taskName)
        if ($task.Enabled -ne $enabledBefore) { throw 'Upgrade recovery changed the original enabled state.' }
        if ($enabledBefore) { Assert-Live } else { Wait-Stopped }
    }
    $task.Enabled = $true
    $null = $task.RunEx($null, 4, $sessionId, $null)
    Assert-Installed $InitialVersion

    $personalLog = Join-Path $temp 'scope-conflict.log'
    $personal = Start-Process -FilePath $UpgradeInstallerPath -Wait -PassThru -ArgumentList `
        ('/CURRENTUSER /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /LOG="' + $personalLog + '"')
    if ($personal.ExitCode -eq 0) { throw 'Personal installation was allowed over the managed installation.' }
    if (Test-Path (Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper\quesma-shipper.exe')) {
        throw 'Scope conflict left a personal executable behind.'
    }
    Assert-Installed $InitialVersion

    $task = $folder.GetTask($taskName)
    $null = $task.RunEx($null, 4, $sessionId, $null)
    Start-Sleep -Seconds 2
    Assert-Live

    $task.Enabled = $false
    $task.Stop(0)
    Wait-Stopped
    Assert-Detected $InitialVersion $false
    $task.Enabled = $true
    Remove-Item -LiteralPath $supervisorPath
    Assert-Detected $InitialVersion $false
    $task.Enabled = $false
    Invoke-Setup $InstallerPath
    Assert-Installed $InitialVersion

    $installScript = (Resolve-Path (Join-Path $PSScriptRoot '..\intune\Install-System.ps1')).Path
    $code = Invoke-SystemProcess "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" `
        ('-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $installScript +
         '" -InstallerPath "' + $UpgradeInstallerPath + '" -Version "' + $ExpectedVersion + '"')
    if ($code -ne 0) { throw "Intune upgrade wrapper exited $code" }
    Assert-Installed $ExpectedVersion
    $banner = & $shipperPath --version
    if ($LASTEXITCODE -ne 0 -or $banner -notlike "quesma-shipper $ExpectedVersion (*") {
        throw "Upgraded executable has unexpected version: $banner"
    }
    if ((Get-SystemState) -ne $systemState) { throw 'Installation wrote shipper state into a service account profile.' }

    $task = $folder.GetTask($taskName)
    $task.Enabled = $false
    $task.Stop(0)
    Wait-Stopped
    Set-Content -LiteralPath $shipperPath -Value 'damaged executable'
    Remove-Item -LiteralPath $supervisorPath
    $uninstallScript = (Resolve-Path (Join-Path $PSScriptRoot '..\intune\Uninstall-System.ps1')).Path
    $uninstallArguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $uninstallScript + '"'
    $lockedPayload = [IO.File]::Open($shipperPath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        $code = Invoke-SystemProcess "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" $uninstallArguments
        if ($code -eq 0) { throw 'Uninstall reported success while the main executable could not be deleted.' }
        if (-not (Test-Path -LiteralPath $shipperPath) -or -not (Test-Path -LiteralPath (Join-Path $installDir 'unins000.exe'))) {
            throw 'Failed uninstall did not retain the files needed for retry.'
        }
        $retryRegistration = $nativeRegistry.OpenSubKey('Software\Quesma\Shipper')
        if (-not $retryRegistration) { throw 'Failed uninstall removed the machine registration.' }
        try {
            if ($retryRegistration.GetValue('InstallDir') -ne $installDir -or
                $retryRegistration.GetValue('Version') -ne $ExpectedVersion) {
                throw 'Failed uninstall changed the machine registration.'
            }
        } finally { $retryRegistration.Dispose() }
    } finally { $lockedPayload.Dispose() }
    $code = Invoke-SystemProcess "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" `
        $uninstallArguments
    if ($code -ne 0) { throw "Machine uninstaller exited $code" }
    $remainingTask = @($folder.GetTasks(0) | Where-Object Name -eq $taskName)
    if ($remainingTask.Count -or (Test-Path -LiteralPath $shipperPath) -or (Test-Path -LiteralPath $supervisorPath) -or
        @(Get-ManagedProcesses).Count) { throw 'Managed uninstall left the task, program, or collector running.' }
    $remainingKey = $nativeRegistry.OpenSubKey('Software\Quesma\Shipper')
    if ($remainingKey) { $remainingKey.Dispose(); throw 'Managed uninstall left its registration behind.' }
    Assert-Detected $ExpectedVersion $false
    if ((Get-Content -LiteralPath $stateMarker -Raw).Trim() -ne 'preserve-user-state') {
        throw 'Managed uninstall removed user state.'
    }
    Write-Output 'Passed: SYSTEM install/detection, user collection, upgrade recovery, scope conflict, duplicate launch, repair, version upgrade, retryable SYSTEM uninstall, and state retention.'
} catch {
    Get-ChildItem -LiteralPath $temp -Filter '*.log' -ErrorAction SilentlyContinue | ForEach-Object {
        Write-Host "Installer log: $($_.Name)"
        Get-Content -LiteralPath $_.FullName | Write-Host
    }
    throw
} finally {
    $nativeRegistry.Dispose()
    Remove-Item -LiteralPath $stateMarker -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
$global:LASTEXITCODE = 0
