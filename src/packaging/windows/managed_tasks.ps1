param(
    [ValidateSet('Prepare', 'Resume', 'Install', 'Remove')][string]$Operation,
    [string]$InstallDir,
    [string]$RecoveryFile
)
$ErrorActionPreference = 'Stop'
$taskName = 'Quesma Shipper Managed'
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$runner = Join-Path $InstallDir 'quesma-shipper-supervisor.exe'

function Get-ManagedTask {
    foreach ($entry in $folder.GetTasks(1)) {
        if ($entry.Name -eq $taskName) {
            if ($entry.Definition.Actions.Count -ne 1 -or
                $entry.Definition.Actions.Item(1).Path -ine $runner -or
                $entry.Definition.Actions.Item(1).Arguments -cne '--managed') {
                throw 'The managed task name is owned by another installation.'
            }
            return $entry
        }
    }
    return $null
}

function Assert-NoPersonalInstall {
    foreach ($entry in $folder.GetTasks(1)) {
        if ($entry.Name -eq 'Quesma Shipper' -or $entry.Name -like 'Quesma Shipper - S-*') {
            throw 'A personal Quesma Shipper task exists. Uninstall that installation without purging its state before switching scope.'
        }
    }
    $profiles = Get-ChildItem 'Registry::HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList'
    foreach ($profile in $profiles) {
        $homePath = (Get-ItemProperty -LiteralPath $profile.PSPath).ProfileImagePath
        if ($homePath) {
            $homePath = [Environment]::ExpandEnvironmentVariables($homePath)
            $personal = Join-Path $homePath 'AppData\Local\Programs\Quesma Shipper'
            if ((Test-Path -LiteralPath (Join-Path $personal 'quesma-shipper.exe')) -or
                (Test-Path -LiteralPath (Join-Path $personal 'unins000.exe'))) {
                throw 'A personal Quesma Shipper installation exists. Uninstall it without purging state before switching scope.'
            }
        }
    }
    foreach ($hive in Get-ChildItem 'Registry::HKEY_USERS') {
        foreach ($key in @('Software\Microsoft\Windows\CurrentVersion\Uninstall', 'Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall')) {
            $uninstall = $hive.PSPath + '\' + $key + '\{C65D423E-3F9B-49AF-A68F-08C88093F01F}_is1'
            if (Test-Path -LiteralPath $uninstall) { throw 'A personal Quesma Shipper uninstall registration exists; remove it before switching scope.' }
        }
    }
}

function Stop-ManagedTask($Task) {
    $Task.Enabled = $false
    $Task.Stop(0)
    $deadline = (Get-Date).AddSeconds(30)
    while ($Task.GetInstances(0).Count -ne 0) {
        if ((Get-Date) -gt $deadline) { throw 'Managed collectors did not stop; retry setup after closing user sessions.' }
        Start-Sleep -Milliseconds 100
    }
}

function Start-UserSessions($Task) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class QuesmaSessions {
    [StructLayout(LayoutKind.Sequential)] public struct Session {
        public int Id; public IntPtr Station; public int State;
    }
    [DllImport("wtsapi32.dll", SetLastError=true)] public static extern bool WTSEnumerateSessionsW(IntPtr server, int reserved, int version, out IntPtr sessions, out int count);
    [DllImport("wtsapi32.dll", SetLastError=true)] public static extern bool WTSQuerySessionInformationW(IntPtr server, int session, int info, out IntPtr buffer, out int bytes);
    [DllImport("wtsapi32.dll")] public static extern void WTSFreeMemory(IntPtr memory);
}
'@
    $sessions = [IntPtr]::Zero
    $count = 0
    if (-not [QuesmaSessions]::WTSEnumerateSessionsW([IntPtr]::Zero, 0, 1, [ref]$sessions, [ref]$count)) {
        throw 'Could not enumerate interactive Windows sessions.'
    }
    try {
        $size = [Runtime.InteropServices.Marshal]::SizeOf([type][QuesmaSessions+Session])
        for ($i = 0; $i -lt $count; $i++) {
            $session = [Runtime.InteropServices.Marshal]::PtrToStructure([IntPtr]::Add($sessions, $i * $size), [type][QuesmaSessions+Session])
            if ($session.Id -le 0 -or $session.State -notin @(0, 1, 4)) { continue }
            $buffer = [IntPtr]::Zero
            $bytes = 0
            if (-not [QuesmaSessions]::WTSQuerySessionInformationW([IntPtr]::Zero, $session.Id, 5, [ref]$buffer, [ref]$bytes)) { continue }
            try { $userName = [Runtime.InteropServices.Marshal]::PtrToStringUni($buffer) }
            finally { [QuesmaSessions]::WTSFreeMemory($buffer) }
            if (-not $userName) { continue }
            try { $null = $Task.RunEx($null, 4, $session.Id, $null) }
            catch { [Console]::Error.WriteLine("Could not start Quesma Shipper in session $($session.Id); it will start at the next logon.") }
        }
    } finally { [QuesmaSessions]::WTSFreeMemory($sessions) }
}

try {
    $task = Get-ManagedTask
    switch ($Operation) {
        'Prepare' {
            Assert-NoPersonalInstall
            if (-not $RecoveryFile) { throw 'Missing setup recovery file.' }
            if (-not (Test-Path -LiteralPath $RecoveryFile)) {
                @{ Existed = [bool]$task; Enabled = [bool]($task -and $task.Enabled) } |
                    ConvertTo-Json -Compress | Set-Content -LiteralPath $RecoveryFile -Encoding UTF8
            }
            if ($task) { Stop-ManagedTask $task }
        }
        'Resume' {
            if (-not (Test-Path -LiteralPath $RecoveryFile)) { exit 0 }
            $previous = Get-Content -LiteralPath $RecoveryFile -Raw | ConvertFrom-Json
            if ($previous.Existed -isnot [bool] -or $previous.Enabled -isnot [bool]) { throw 'Invalid setup recovery file; rerun the all-users installer.' }
            if ($previous.Existed) {
                if (-not $task) { throw 'The previous managed task is missing; rerun the all-users installer to repair it.' }
                $task.Enabled = [bool]$previous.Enabled
                if ($task.Enabled) { Start-UserSessions $task }
            } elseif ($task) {
                Stop-ManagedTask $task
                $folder.DeleteTask($taskName, 0)
            }
            Remove-Item -LiteralPath $RecoveryFile -Force
        }
        'Install' {
            Assert-NoPersonalInstall
            $definition = $scheduler.NewTask(0)
            $definition.RegistrationInfo.Description = 'Quesma Shipper: managed program, separate collector for each interactive user.'
            $definition.Principal.GroupId = 'S-1-5-32-545'
            $definition.Principal.RunLevel = 0
            $trigger = $definition.Triggers.Create(9)
            $trigger.Delay = 'PT30S'
            $trigger.Repetition.Interval = 'PT1H'
            $definition.Settings.MultipleInstances = 0
            $definition.Settings.ExecutionTimeLimit = 'PT0S'
            $definition.Settings.DisallowStartIfOnBatteries = $false
            $definition.Settings.StopIfGoingOnBatteries = $false
            $definition.Settings.StartWhenAvailable = $true
            $definition.Settings.RestartInterval = 'PT15M'
            $definition.Settings.RestartCount = 3
            $action = $definition.Actions.Create(0)
            $action.Path = $runner
            $action.Arguments = '--managed'
            # Readers cannot stop every user's instance; suppress Task Scheduler's implicit principal ACE.
            $task = $folder.RegisterTaskDefinition($taskName, $definition, 22, $null, $null, 4,
                'D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;IU)')
            Start-UserSessions $task
        }
        'Remove' {
            if ($task) {
                Stop-ManagedTask $task
                $folder.DeleteTask($taskName, 0)
            }
        }
    }
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
