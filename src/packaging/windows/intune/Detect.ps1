# Use the same server and exact organization name as the private Install.ps1 copy.
[CmdletBinding()]
param(
    [string]$Server = 'https://cp.example.com',
    [string]$Organization = 'Example Organization'
)

$ErrorActionPreference = 'Stop'
try {
    if ($Server -eq 'https://cp.example.com' -or $Organization -eq 'Example Organization') { exit 1 }
    $exe = Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper\quesma-shipper.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { exit 1 }
    $supervisor = Join-Path (Split-Path $exe) 'quesma-shipper-supervisor.exe'
    if (-not (Test-Path -LiteralPath $supervisor -PathType Leaf)) { exit 1 }
    $raw = & $exe status --json 2>$null
    if ($LASTEXITCODE -ne 0) { exit 1 }
    $status = ($raw -join "`n") | ConvertFrom-Json
    if ($status.logged_in -and $status.endpoint -and
        $status.endpoint.TrimEnd('/') -ceq $Server.TrimEnd('/') -and
        $status.organization -ceq $Organization -and
        $status.service -eq 'windows-task' -and $status.service_ok) {
        Write-Output 'Quesma Shipper installed and enrolled.'
        exit 0
    }
} catch {}
exit 1
