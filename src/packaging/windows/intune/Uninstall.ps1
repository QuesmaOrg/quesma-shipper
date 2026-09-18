# Remove this user's task and program through Inno Setup, preserving enrollment and state.
$ErrorActionPreference = 'Stop'
try {
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    if ($sid -in @('S-1-5-18', 'S-1-5-19', 'S-1-5-20')) {
        throw 'Use User install behavior; this uninstaller removes the current user installation.'
    }
    $dir = Join-Path $env:LOCALAPPDATA 'Programs\Quesma Shipper'
    $uninstaller = Join-Path $dir 'unins000.exe'
    if (-not (Test-Path -LiteralPath $uninstaller -PathType Leaf)) {
        if (Test-Path -LiteralPath (Join-Path $dir 'quesma-shipper.exe')) {
            throw 'Shipper exists without its setup uninstaller; inspect the installation manually.'
        }
        exit 0
    }
    $process = Start-Process -FilePath $uninstaller -Wait -PassThru `
        -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART'
    exit $process.ExitCode
} catch {
    Write-Error $_
    exit 1
}
