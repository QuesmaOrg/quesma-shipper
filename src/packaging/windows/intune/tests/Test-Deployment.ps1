# Synthetic executables and setup exercise repair and detection without installing real software.
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot
$shell = (Get-Process -Id $PID).Path

foreach ($file in Get-ChildItem $root -Filter *.ps1) {
    $tokens = $null
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        $file.FullName, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
    if ($file.Name -eq 'Install.ps1') {
        $definitions = $ast.FindAll({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
        }, $true)
        foreach ($definition in $definitions) { Invoke-Expression $definition.Extent.Text }
    }
}

foreach ($case in @(
    @{ Status = @{}; Reject = $false },
    @{ Status = @{ endpoint = 'https://cp.example.com'; organization = 'Test Organization' }; Reject = $false },
    @{ Status = @{ endpoint = 'https://cp.example.com/'; organization = 'Test Organization' }; Reject = $false },
    @{ Status = @{ endpoint = 'https://other.example.com'; organization = 'Test Organization' }; Reject = $true },
    @{ Status = @{ endpoint = 'https://cp.example.com'; organization = 'Other Organization' }; Reject = $true },
    @{ Status = @{ endpoint = 'https://cp.example.com' }; Reject = $true }
)) {
    $rejected = $false
    try { Assert-EnrollmentTarget ([pscustomobject]$case.Status) 'https://cp.example.com' 'Test Organization' }
    catch { $rejected = $true }
    if ($rejected -ne $case.Reject) { throw 'Enrollment target guard returned an unexpected result.' }
}

$temp = Join-Path ([IO.Path]::GetTempPath()) ('quesma-intune-test-' + [guid]::NewGuid())
$savedLocalAppData = $env:LOCALAPPDATA
try {
    $env:LOCALAPPDATA = $temp
    $exe = Join-Path $temp 'Programs\Quesma Shipper\quesma-shipper.exe'
    New-Item -ItemType Directory -Path (Split-Path $exe) -Force | Out-Null
    $stub = Join-Path $temp 'status.go'
    @'
package main
import ("fmt"; "os"; "strconv")
func main() {
    if len(os.Args) == 4 && os.Args[1] == "login" && os.Args[2] == "--server" {
        if os.Getenv("SHIPPER_AUTH_KEY") != "synthetic-grant" { os.Exit(8) }
        if err := os.WriteFile(os.Getenv("QUESMA_INTUNE_TEST_LOGIN"), []byte(os.Getenv("QUESMA_INTUNE_TEST_ENROLLED")), 0600); err != nil { panic(err) }
        return
    }
    if len(os.Args) != 3 || os.Args[1] != "status" || os.Args[2] != "--json" { os.Exit(9) }
    status := os.Getenv("QUESMA_INTUNE_TEST_STATUS")
    if raw, err := os.ReadFile(os.Getenv("QUESMA_INTUNE_TEST_LOGIN")); err == nil { status = string(raw) }
    fmt.Println(status)
    code, _ := strconv.Atoi(os.Getenv("QUESMA_INTUNE_TEST_EXIT"))
    os.Exit(code)
}
'@ | Set-Content -LiteralPath $stub -Encoding ASCII
    & go build -o $exe $stub
    if ($LASTEXITCODE -ne 0) { throw 'Could not build the synthetic shipper.' }
    $template = Join-Path $temp 'shipper-template.exe'
    Copy-Item $exe $template
    $supervisor = Join-Path (Split-Path $exe) 'quesma-shipper-supervisor.exe'
    Set-Content -LiteralPath $supervisor -Value 'synthetic supervisor'

    $good = @{
        logged_in = $true; endpoint = 'https://control.example.com'
        organization = 'Test Organization'; service = 'windows-task'; service_ok = $true
    }
    $cases = @(
        @{ Name = 'enrolled'; Patch = @{}; Exit = 0; Detected = $true },
        @{ Name = 'identity only'; Patch = @{ endpoint = ''; organization = '' }; Exit = 0; Detected = $false },
        @{ Name = 'wrong server'; Patch = @{ endpoint = 'https://other.example.com' }; Exit = 0; Detected = $false },
        @{ Name = 'wrong organization'; Patch = @{ organization = 'Other Organization' }; Exit = 0; Detected = $false },
        @{ Name = 'missing identity'; Patch = @{ logged_in = $false }; Exit = 0; Detected = $false },
        @{ Name = 'missing task'; Patch = @{ service = 'not installed' }; Exit = 0; Detected = $false },
        @{ Name = 'disabled task'; Patch = @{ service = 'installed but not running'; service_ok = $false }; Exit = 0; Detected = $false },
        @{ Name = 'service not OK'; Patch = @{ service_ok = $false }; Exit = 0; Detected = $false },
        @{ Name = 'different task kind'; Patch = @{ service = 'unexpected' }; Exit = 0; Detected = $false },
        @{ Name = 'status failure'; Patch = @{}; Exit = 2; Detected = $false },
        @{ Name = 'invalid JSON'; Patch = @{}; Exit = 0; Detected = $false },
        @{ Name = 'missing executable'; Patch = @{}; Exit = 0; Detected = $false },
        @{ Name = 'missing supervisor'; Patch = @{}; Exit = 0; Detected = $false }
    )
    foreach ($case in $cases) {
        Copy-Item $template $exe -Force
        Set-Content -LiteralPath $supervisor -Value 'synthetic supervisor'
        $status = $good.Clone()
        foreach ($key in $case.Patch.Keys) { $status[$key] = $case.Patch[$key] }
        $env:QUESMA_INTUNE_TEST_STATUS = $status | ConvertTo-Json -Compress
        $env:QUESMA_INTUNE_TEST_EXIT = [string]$case.Exit
        if ($case.Name -eq 'invalid JSON') { $env:QUESMA_INTUNE_TEST_STATUS = 'invalid' }
        if ($case.Name -eq 'missing executable') { Remove-Item -LiteralPath $exe }
        if ($case.Name -eq 'missing supervisor') { Remove-Item -LiteralPath $supervisor }
        $output = & $shell -NoProfile -File (Join-Path $root 'Detect.ps1') `
            -Server 'https://control.example.com' -Organization 'Test Organization'
        $code = $LASTEXITCODE
        $expectedCode = if ($case.Detected) { 0 } else { 1 }
        if ($code -ne $expectedCode -or [bool]$output -ne $case.Detected) {
            throw "Detection failed: $($case.Name), exit=$code, output=$output"
        }
    }
    Write-Output "Passed: script parsing, 6 enrollment guards, $($cases.Count) detection scenarios."
    & (Join-Path $PSScriptRoot 'Test-Install.ps1') -Temp $temp -Template $template -Good $good
} finally {
    $env:LOCALAPPDATA = $savedLocalAppData
    Remove-Item Env:QUESMA_INTUNE_TEST_STATUS, Env:QUESMA_INTUNE_TEST_EXIT,
        Env:QUESMA_INTUNE_TEST_LOGIN, Env:QUESMA_INTUNE_TEST_ENROLLED -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
$global:LASTEXITCODE = 0
