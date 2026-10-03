# Detection must succeed before first login and reject incomplete or misconfigured machine installs.
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $root 'Detect-System.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($definition in $ast.FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
}, $true)) { Invoke-Expression $definition.Extent.Text }

$temp = Join-Path ([IO.Path]::GetTempPath()) ('quesma-system-detection-' + [guid]::NewGuid())
try {
    New-Item -ItemType Directory -Path $temp | Out-Null
    $version = '1.2.3-4.test'
    $runner = [Security.SecurityElement]::Escape((Join-Path $temp 'quesma-shipper-supervisor.exe'))
    $good = @"
<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals><Principal id="Users"><GroupId>S-1-5-32-545</GroupId><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Settings><Enabled>true</Enabled><MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy></Settings>
  <Actions Context="Users"><Exec><Command>$runner</Command><Arguments>--managed</Arguments></Exec></Actions>
</Task>
"@
    $cases = @(
        @{ Name = 'installed before first login'; Detect = $true },
        @{ Name = 'default enabled'; Replace = @('<Enabled>true</Enabled>', ''); Detect = $true },
        @{ Name = 'default least privilege'; Replace = @('<RunLevel>LeastPrivilege</RunLevel>', ''); Detect = $true },
        @{ Name = 'empty run level'; Replace = @('LeastPrivilege', '') },
        @{ Name = 'wrong version'; Version = '1.2.3-3.previous' },
        @{ Name = 'unconfigured version'; ExpectedVersion = 'REPLACE_WITH_RELEASE_VERSION' },
        @{ Name = 'registration missing'; MissingRegistration = $true },
        @{ Name = 'registration outside Program Files'; Directory = (Join-Path $temp 'personal') },
        @{ Name = 'shipper missing'; MissingFile = 'quesma-shipper.exe' },
        @{ Name = 'supervisor missing'; MissingFile = 'quesma-shipper-supervisor.exe' },
        @{ Name = 'uninstaller missing'; MissingFile = 'unins000.exe' },
        @{ Name = 'disabled'; Replace = @('<Enabled>true</Enabled>', '<Enabled>false</Enabled>') },
        @{ Name = 'serialized user sessions'; Replace = @('Parallel', 'IgnoreNew') },
        @{ Name = 'elevated collector'; Replace = @('LeastPrivilege', 'HighestAvailable') },
        @{ Name = 'wrong group'; Replace = @('S-1-5-32-545', 'S-1-5-32-544') },
        @{ Name = 'explicit user'; Replace = @('</GroupId>', '</GroupId><UserId>S-1-5-21-1</UserId>') },
        @{ Name = 'restricted logon trigger'; Replace = @('<LogonTrigger>', '<LogonTrigger><UserId>S-1-5-21-1</UserId>') },
        @{ Name = 'startup trigger'; Replace = @('LogonTrigger', 'BootTrigger') },
        @{ Name = 'wrong executable'; Replace = @($runner, 'C:\untrusted\runner.exe') },
        @{ Name = 'personal arguments'; Replace = @('--managed', 'C:\Users\old-user\logs') },
        @{ Name = 'extra action'; Replace = @('</Exec>', '</Exec><Exec><Command>bad.exe</Command></Exec>') },
        @{ Name = 'invalid task XML'; XML = 'broken' }
    )
    foreach ($case in $cases) {
        foreach ($name in @('quesma-shipper.exe', 'quesma-shipper-supervisor.exe', 'unins000.exe')) {
            Set-Content -LiteralPath (Join-Path $temp $name) -Value 'synthetic file; must never be executed'
        }
        if ($case.MissingFile) { Remove-Item -LiteralPath (Join-Path $temp $case.MissingFile) }
        $registration = [pscustomobject]@{ Directory = $temp; ExpectedDirectory = $temp; Version = $version }
        if ($case.Version) { $registration.Version = $case.Version }
        if ($case.Directory) { $registration.Directory = $case.Directory }
        if ($case.MissingRegistration) { $registration = $null }
        $expected = if ($case.ExpectedVersion) { $case.ExpectedVersion } else { $version }
        $xml = if ($case.XML) { $case.XML } else { $good }
        if ($case.Replace) { $xml = $xml.Replace($case.Replace[0], $case.Replace[1]) }
        $detected = $false
        try { $detected = Test-SystemInstallation $registration $xml $expected } catch {}
        if ($detected -ne [bool]$case.Detect) { throw "Unexpected machine detection result: $($case.Name)." }
    }
    Write-Output "Passed: $($cases.Count) machine installation detection scenarios."
} finally {
    Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue
}
