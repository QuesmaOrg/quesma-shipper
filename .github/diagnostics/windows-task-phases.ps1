$ErrorActionPreference = 'Stop'
git rev-parse HEAD
go version
Write-Host "Mode=$env:DIAGNOSTIC_MODE Image=$env:ImageVersion CPU=$env:NUMBER_OF_PROCESSORS UTC=$([DateTime]::UtcNow.ToString('o'))"
Get-CimInstance Win32_Service -Filter "Name='Schedule'" | Select-Object Name,State,ProcessId,StartMode | Format-List
# Change only disposable checkout diagnostics: original query, original timeout and exit handling.
$file = 'src/packaging/windows/managed_windows.go'
$raw = Get-Content $file -Raw
$raw = $raw.Replace("`$ErrorActionPreference = 'Stop'; `$s = New-Object", "`$ErrorActionPreference = 'Stop'; [Console]::Error.WriteLine('PHASE script-enter ' + [DateTime]::UtcNow.ToString('o')); `$s = New-Object")
$raw = $raw.Replace("; `$s.Connect(); ", "; [Console]::Error.WriteLine('PHASE object-created ' + [DateTime]::UtcNow.ToString('o')); `$s.Connect(); [Console]::Error.WriteLine('PHASE connected ' + [DateTime]::UtcNow.ToString('o')); ")
$raw = $raw.Replace("`$f = `$s.GetFolder('\'); try", "`$f = `$s.GetFolder('\'); [Console]::Error.WriteLine('PHASE folder-open ' + [DateTime]::UtcNow.ToString('o')); try")
$raw = $raw.Replace("if (`$e.HResult -eq -2147024894)", "[Console]::Error.WriteLine('PHASE HRESULT=' + `$e.HResult + ' ' + [DateTime]::UtcNow.ToString('o')); if (`$e.HResult -eq -2147024894)")
$raw = $raw.Replace('out, err := cmd.CombinedOutput()', "out, err := cmd.CombinedOutput()`n fmt.Fprintf(os.Stderr, `"DIAGNOSTIC query end=%s err=%v context=%v output=%s\n`", time.Now().UTC().Format(time.RFC3339Nano), err, ctx.Err(), out)")
Set-Content $file $raw -NoNewline
@'
package windows
import (
 "testing"
 "sync"
 "fmt"
 "time"
)
func TestDiagnosticConcurrentQueries(t *testing.T) {
 var wg sync.WaitGroup
 for i:=0; i<32; i++ { wg.Add(1); go func(i int) { defer wg.Done(); start:=time.Now(); err:=checkNoManagedTask(); fmt.Printf("CONCURRENT i=%d elapsed=%s error=%v\n",i,time.Since(start),err); if err!=nil {t.Errorf("query %d: %v",i,err)} }(i) }; wg.Wait()
}
'@ | Set-Content src/packaging/windows/diagnostic_windows_test.go
Push-Location src
go test -c -o ../task-diagnostic.exe ./packaging/windows
if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
Pop-Location
$test = '^TestManagedTaskConflictQueryUsesTaskSchedulerWithoutModifyingIt$'
if ($env:DIAGNOSTIC_MODE -eq 'contention') {
  ./task-diagnostic.exe -test.run '^TestDiagnosticConcurrentQueries$' -test.count=3 -test.v -test.timeout=5m
  Write-Host "CONCURRENT exit=$LASTEXITCODE"
} else {
  ./task-diagnostic.exe -test.run $test -test.count=10 -test.v -test.timeout=7m
  Write-Host "SEQUENTIAL exit=$LASTEXITCODE"
}
if ($env:DIAGNOSTIC_MODE -eq 'contention') {
  # Controlled RPC unavailability, never claimed as reconstruction of the incident.
  $svc = Get-CimInstance Win32_Service -Filter "Name='Schedule'"
  $shared = @(Get-CimInstance Win32_Service | Where-Object { $_.ProcessId -eq $svc.ProcessId -and $_.State -eq 'Running' })
  if ($svc.State -ne 'Running' -or $svc.ProcessId -le 0 -or $shared.Count -ne 1) {
    Write-Host 'CONTROLLED BLOCKER: Scheduler not isolated in its own process; no suspension attempted.'
  } else {
    Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class DiagnosticNative {
 [DllImport("kernel32.dll", SetLastError=true)] public static extern IntPtr OpenProcess(uint access,bool inherit,uint pid);
 [DllImport("kernel32.dll")] public static extern bool CloseHandle(IntPtr h);
 [DllImport("ntdll.dll")] public static extern int NtSuspendProcess(IntPtr h);
 [DllImport("ntdll.dll")] public static extern int NtResumeProcess(IntPtr h);
}
'@
    $h = [DiagnosticNative]::OpenProcess(0x0800, $false, $svc.ProcessId)
    if ($h -eq [IntPtr]::Zero) { throw 'Could not open Schedule process for controlled suspension' }
    try {
      $status = [DiagnosticNative]::NtSuspendProcess($h)
      if ($status -ne 0) { throw "Suspend failed: $status" }
      Write-Host "CONTROLLED Schedule PID=$($svc.ProcessId) suspended UTC=$([DateTime]::UtcNow.ToString('o'))"
      ./task-diagnostic.exe -test.run $test -test.v -test.timeout=45s
      Write-Host "CONTROLLED expected failure exit=$LASTEXITCODE"
    } finally {
      $resume = [DiagnosticNative]::NtResumeProcess($h)
      [DiagnosticNative]::CloseHandle($h) | Out-Null
      Write-Host "CONTROLLED resume status=$resume UTC=$([DateTime]::UtcNow.ToString('o'))"
    }
    ./task-diagnostic.exe -test.run $test -test.count=2 -test.v -test.timeout=90s
    Write-Host "RECOVERY exit=$LASTEXITCODE"
  }
}
Get-CimInstance Win32_Service -Filter "Name='Schedule'" | Select-Object Name,State,ProcessId | Format-List
exit 0
