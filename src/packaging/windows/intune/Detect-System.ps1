# Intune detection checks device installation, independently of user enrollment.
[CmdletBinding()]
param([string]$Version = 'REPLACE_WITH_RELEASE_VERSION')

$ErrorActionPreference = 'Stop'

function Get-SystemRegistration {
    $base = [Microsoft.Win32.RegistryKey]::OpenBaseKey(
        [Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryView]::Registry64)
    try {
        $installation = $base.OpenSubKey('Software\Quesma\Shipper')
        $windows = $base.OpenSubKey('Software\Microsoft\Windows\CurrentVersion')
        try {
            if (-not $installation -or -not $windows) { return $null }
            foreach ($name in @('InstallDir', 'Version')) {
                if ($installation.GetValueKind($name) -ne [Microsoft.Win32.RegistryValueKind]::String) { return $null }
            }
            return [pscustomobject]@{
                Directory = [string]$installation.GetValue('InstallDir')
                Version = [string]$installation.GetValue('Version')
                ExpectedDirectory = Join-Path ([string]$windows.GetValue('ProgramFilesDir')) 'Quesma Shipper'
            }
        } finally {
            if ($installation) { $installation.Dispose() }
            if ($windows) { $windows.Dispose() }
        }
    } finally { $base.Dispose() }
}

function Test-SystemInstallation($Registration, [string]$TaskXML, [string]$ExpectedVersion) {
    if (-not $Registration -or [string]::IsNullOrWhiteSpace($ExpectedVersion) -or
        $ExpectedVersion -eq 'REPLACE_WITH_RELEASE_VERSION' -or
        $Registration.Version -cne $ExpectedVersion -or
        $Registration.Directory.TrimEnd('\') -ine $Registration.ExpectedDirectory.TrimEnd('\')) {
        return $false
    }
    foreach ($name in @('quesma-shipper.exe', 'quesma-shipper-supervisor.exe', 'unins000.exe')) {
        if (-not (Test-Path -LiteralPath (Join-Path $Registration.Directory $name) -PathType Leaf)) {
            return $false
        }
    }
    $settings = New-Object System.Xml.XmlReaderSettings
    $settings.DtdProcessing = [System.Xml.DtdProcessing]::Prohibit
    $reader = [System.Xml.XmlReader]::Create((New-Object System.IO.StringReader($TaskXML)), $settings)
    try {
        $document = New-Object System.Xml.XmlDocument
        $document.XmlResolver = $null
        $document.Load($reader)
    } finally { $reader.Dispose() }
    $ns = New-Object System.Xml.XmlNamespaceManager($document.NameTable)
    $ns.AddNamespace('t', 'http://schemas.microsoft.com/windows/2004/02/mit/task')
    $principals = $document.SelectNodes('/t:Task/t:Principals/t:Principal', $ns)
    $actions = $document.SelectNodes('/t:Task/t:Actions/*', $ns)
    $triggers = $document.SelectNodes('/t:Task/t:Triggers/*', $ns)
    if ($principals.Count -ne 1 -or $actions.Count -ne 1 -or $triggers.Count -ne 1) { return $false }
    $principal = $principals[0]
    $action = $actions[0]
    $trigger = $triggers[0]
    $group = [string]$principal.GroupId
    if ($group -and $group -notmatch '^S-') {
        $account = New-Object Security.Principal.NTAccount($group)
        $group = $account.Translate([Security.Principal.SecurityIdentifier]).Value
    }
    return ($group -eq 'S-1-5-32-545' -and -not $principal.UserId -and
        $principal.RunLevel -eq 'LeastPrivilege' -and
        $action.LocalName -eq 'Exec' -and
        [string]$action.Command -ieq (Join-Path $Registration.Directory 'quesma-shipper-supervisor.exe') -and
        [string]$action.Arguments -ceq '--managed' -and
        $trigger.LocalName -eq 'LogonTrigger' -and -not $trigger.UserId -and
        $trigger.Enabled -ne 'false' -and $document.Task.Settings.Enabled -ne 'false' -and
        $document.Task.Settings.MultipleInstancesPolicy -eq 'Parallel')
}

try {
    $registration = Get-SystemRegistration
    $scheduler = New-Object -ComObject 'Schedule.Service'
    $scheduler.Connect()
    $task = $scheduler.GetFolder('\').GetTask('Quesma Shipper Managed')
    if ($task.Enabled -and (Test-SystemInstallation $registration $task.Xml $Version)) {
        Write-Output "Quesma Shipper $Version is installed and enabled for all users."
        exit 0
    }
} catch {}
exit 1
