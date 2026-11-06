$ErrorActionPreference = 'Stop'
Write-Host "IMPLEMENTATION=$env:DIAGNOSTIC_IMPLEMENTATION IMAGE=$env:ImageVersion CPU=$env:NUMBER_OF_PROCESSORS UTC=$([DateTime]::UtcNow.ToString('o'))"
git rev-parse HEAD
go version
$svc = Get-CimInstance Win32_Service -Filter "Name='Schedule'"
$shared = @(Get-CimInstance Win32_Service | Where-Object { $_.ProcessId -eq $svc.ProcessId -and $_.State -eq 'Running' })
if ($svc.State -ne 'Running' -or $svc.ProcessId -le 0 -or $shared.Count -ne 1) {
  throw 'Cannot safely isolate Schedule service process for disposable-runner diagnostics'
}
$native = @'
using System;
using System.Runtime.InteropServices;
public static class RecoveryNative {
 [DllImport("kernel32.dll", SetLastError=true)] public static extern IntPtr OpenProcess(uint access,bool inherit,uint pid);
 [DllImport("kernel32.dll")] public static extern bool CloseHandle(IntPtr h);
 [DllImport("ntdll.dll")] public static extern int NtSuspendProcess(IntPtr h);
 [DllImport("ntdll.dll")] public static extern int NtResumeProcess(IntPtr h);
}
'@
Add-Type $native
# Phase logging only: the queried task, per-process timeout and error handling remain unchanged.
$file = 'src/packaging/windows/managed_windows.go'
$raw = Get-Content $file -Raw
$raw = $raw.Replace("`$ErrorActionPreference = 'Stop'; `$s = New-Object", "`$ErrorActionPreference = 'Stop'; [Console]::Error.WriteLine('PHASE script-enter ' + [DateTime]::UtcNow.ToString('o')); `$s = New-Object")
$raw = $raw.Replace("; `$s.Connect(); ", "; [Console]::Error.WriteLine('PHASE object-created ' + [DateTime]::UtcNow.ToString('o')); `$s.Connect(); [Console]::Error.WriteLine('PHASE connected ' + [DateTime]::UtcNow.ToString('o')); ")
$raw = $raw.Replace('out, err := cmd.CombinedOutput()', "out, err := cmd.CombinedOutput()`n fmt.Fprintf(os.Stderr, `"QUERY end=%s err=%v context=%v output=%s\n`", time.Now().UTC().Format(time.RFC3339Nano), err, ctx.Err(), out)")
Set-Content $file $raw -NoNewline
@'
package windows
import (
 "context"
 "errors"
 "fmt"
 "os"
 "testing"
 "time"
 "github.com/QuesmaOrg/quesma-shipper/packaging/common"
)
func TestDiagnosticTaskState(t *testing.T) {
 start:=time.Now()
 err:=checkNoManagedTask()
 expected:=os.Getenv("QUERY_EXPECT")
 fmt.Printf("RESULT case=%s expected=%s elapsed=%s error=%v\n",os.Getenv("QUERY_CASE"),expected,time.Since(start),err)
 switch expected {
 case "absent": if err!=nil {t.Fatal(err)}
 case "managed": if !errors.Is(err,common.ErrSystemManaged) {t.Fatalf("must report managed task, got %v",err)}
 case "timeout": if !errors.Is(err,context.DeadlineExceeded) {t.Fatalf("must fail closed with timeout, got %v",err)}
 case "blocked": if err==nil || errors.Is(err,context.DeadlineExceeded) {t.Fatalf("must fail closed promptly, got %v",err)}
 default: t.Fatalf("invalid expectation %q",expected)
 }
}
'@ | Set-Content src/packaging/windows/diagnostic_recovery_windows_test.go
Push-Location src
go test -c -o ../query-recovery.exe ./packaging/windows
if ($LASTEXITCODE -ne 0) { throw 'Diagnostic build failed' }
Pop-Location

function Invoke-Probe([string]$Case, [string]$Expected, [int]$Count = 1) {
  $env:QUERY_CASE = $Case
  $env:QUERY_EXPECT = $Expected
  ./query-recovery.exe '-test.run=^TestDiagnosticTaskState$' "-test.count=$Count" '-test.v' '-test.timeout=90s'
  if ($LASTEXITCODE -ne 0) { throw "Probe failed: $Case exit=$LASTEXITCODE" }
}

function Invoke-StalledProbe([string]$Case, [string]$RecoveredState, [bool]$Recover) {
  $h = [RecoveryNative]::OpenProcess(0x0800, $false, $svc.ProcessId)
  if ($h -eq [IntPtr]::Zero) { throw 'Cannot open isolated Schedule process' }
  $job = $null
  try {
    if ([RecoveryNative]::NtSuspendProcess($h) -ne 0) { throw 'Cannot suspend Schedule process' }
    Write-Host "SUSPEND case=$Case pid=$($svc.ProcessId) UTC=$([DateTime]::UtcNow.ToString('o'))"
    if ($Recover) {
      $job = Start-Job -ArgumentList $native,$svc.ProcessId -ScriptBlock {
        param($NativeSource,$ServiceProcessId)
        Add-Type $NativeSource
        Start-Sleep -Seconds 35
        $handle = [RecoveryNative]::OpenProcess(0x0800, $false, $ServiceProcessId)
        try {
          $status = [RecoveryNative]::NtResumeProcess($handle)
          Write-Output "TIMED RESUME status=$status UTC=$([DateTime]::UtcNow.ToString('o'))"
          if ($status -ne 0) { throw 'Timed resume failed' }
        } finally { [RecoveryNative]::CloseHandle($handle) | Out-Null }
      }
    }
    $expected = 'timeout'
    if ($Recover -and $env:DIAGNOSTIC_IMPLEMENTATION -eq 'retry') { $expected = $RecoveredState }
    Invoke-Probe $Case $expected
    if ($job) {
      $null = Wait-Job $job -Timeout 50
      Receive-Job $job
      if ($job.State -ne 'Completed') { throw "Recovery job did not complete: $($job.State)" }
    }
  } finally {
    $resume = [RecoveryNative]::NtResumeProcess($h)
    [RecoveryNative]::CloseHandle($h) | Out-Null
    if ($job) { Stop-Job $job; Remove-Job $job }
    Write-Host "FINAL RESUME status=$resume case=$Case UTC=$([DateTime]::UtcNow.ToString('o'))"
  }
  Invoke-Probe "$Case-after-resume" $RecoveredState
}

Invoke-Probe 'healthy-absent' 'absent' 20
Invoke-StalledProbe 'absent-recovery-after-35s' 'absent' $true
Invoke-StalledProbe 'persistent-unavailability' 'absent' $false

# This disabled fixture has no trigger and can never execute during these probes.
$scheduler = New-Object -ComObject Schedule.Service
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$definition = $scheduler.NewTask(0)
$definition.Settings.Enabled = $false
$definition.Principal.UserId = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$definition.Principal.LogonType = 3
$action = $definition.Actions.Create(0)
$action.Path = "$env:SystemRoot\System32\cmd.exe"
$action.Arguments = '/c exit 0'
$task = $folder.RegisterTaskDefinition('Quesma Shipper Managed', $definition, 2, $null, $null, 3)
try {
  Invoke-Probe 'healthy-managed' 'managed' 20
  Invoke-StalledProbe 'managed-recovery-after-35s' 'managed' $true
  $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $task.SetSecurityDescriptor("D:P(D;;GR;;;$sid)(A;;FA;;;SY)(A;;FA;;;BA)", 0)
  Invoke-Probe 'managed-deny-read' 'blocked' 3
} finally {
  $task.SetSecurityDescriptor('D:P(A;;FA;;;SY)(A;;FA;;;BA)', 0)
  $folder.DeleteTask('Quesma Shipper Managed', 0)
}
Invoke-Probe 'fixture-removed' 'absent'
Get-CimInstance Win32_Service -Filter "Name='Schedule'" | Select-Object Name,State,ProcessId | Format-List
