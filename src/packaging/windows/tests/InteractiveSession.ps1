# A loopback RDP connection supplies a real standard-user desktop session on disposable Windows Server runners.
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Security
Add-Type -ReferencedAssemblies System, System.Windows.Forms, System.Drawing -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class QuesmaTestSessions {
    [StructLayout(LayoutKind.Sequential)] struct Session {
        public int Id; public IntPtr Station; public int State;
    }
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSEnumerateSessionsW(IntPtr server, int reserved, int version, out IntPtr sessions, out int count);
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSQuerySessionInformationW(IntPtr server, int session, int info, out IntPtr buffer, out int bytes);
    [DllImport("wtsapi32.dll")] static extern void WTSFreeMemory(IntPtr memory);
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSLogoffSession(IntPtr server, int session, bool wait);
    [StructLayout(LayoutKind.Sequential)] struct PolicyAttributes {
        public uint Length; public IntPtr Root, Name; public uint Attributes; public IntPtr Security, Quality;
    }
    [StructLayout(LayoutKind.Sequential)] struct PolicyString {
        public ushort Length, MaximumLength; public IntPtr Buffer;
    }
    [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr system, ref PolicyAttributes attributes, uint access, out IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr policy, byte[] sid, PolicyString[] rights, uint count);
    [DllImport("advapi32.dll")] static extern uint LsaRemoveAccountRights(IntPtr policy, byte[] sid, bool all, PolicyString[] rights, uint count);
    [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaNtStatusToWinError(uint status);
    public static void RemoteLogonRight(string sid, bool allow) {
        var attributes = new PolicyAttributes { Length = (uint)Marshal.SizeOf(typeof(PolicyAttributes)) };
        IntPtr policy;
        uint status = LsaOpenPolicy(IntPtr.Zero, ref attributes, 0x810, out policy);
        if (status != 0) throw new Win32Exception((int)LsaNtStatusToWinError(status));
        string name = "SeRemoteInteractiveLogonRight";
        IntPtr buffer = Marshal.StringToHGlobalUni(name);
        try {
            var identifier = new System.Security.Principal.SecurityIdentifier(sid);
            var bytes = new byte[identifier.BinaryLength];
            identifier.GetBinaryForm(bytes, 0);
            var rights = new[] { new PolicyString { Length = (ushort)(name.Length * 2), MaximumLength = (ushort)(name.Length * 2 + 2), Buffer = buffer } };
            status = allow ? LsaAddAccountRights(policy, bytes, rights, 1) : LsaRemoveAccountRights(policy, bytes, false, rights, 1);
            if (status != 0) throw new Win32Exception((int)LsaNtStatusToWinError(status));
        } finally { Marshal.FreeHGlobal(buffer); LsaClose(policy); }
    }
    public static int Find(string user) {
        IntPtr sessions; int count;
        if (!WTSEnumerateSessionsW(IntPtr.Zero, 0, 1, out sessions, out count)) throw new Win32Exception();
        try {
            int size = Marshal.SizeOf(typeof(Session));
            for (int i = 0; i < count; i++) {
                var session = (Session)Marshal.PtrToStructure(IntPtr.Add(sessions, i * size), typeof(Session));
                if (session.Id <= 0 || session.State != 0) continue;
                IntPtr buffer; int bytes;
                if (!WTSQuerySessionInformationW(IntPtr.Zero, session.Id, 5, out buffer, out bytes)) continue;
                try { if (String.Equals(Marshal.PtrToStringUni(buffer), user, StringComparison.OrdinalIgnoreCase)) return session.Id; }
                finally { WTSFreeMemory(buffer); }
            }
        } finally { WTSFreeMemory(sessions); }
        return -1;
    }
    public static void Logoff(int session) {
        if (!WTSLogoffSession(IntPtr.Zero, session, true)) throw new Win32Exception();
    }
}
'@

function Open-TestSession([string]$Name, [string]$Password) {
    $rdpFile = Join-Path ([IO.Path]::GetTempPath()) ('quesma-session-' + [guid]::NewGuid() + '.rdp')
    $client = $null
    try {
        $protected = [Security.Cryptography.ProtectedData]::Protect([Text.Encoding]::Unicode.GetBytes($Password),
            $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
        $passwordHex = [BitConverter]::ToString($protected).Replace('-', '')
        New-Item -ItemType File -Path $rdpFile | Out-Null
        $acl = New-Object Security.AccessControl.FileSecurity
        $acl.SetAccessRuleProtection($true, $false)
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
            [Security.Principal.WindowsIdentity]::GetCurrent().User, 'FullControl', 'Allow')))
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
            (New-Object Security.Principal.SecurityIdentifier('S-1-5-18')), 'FullControl', 'Allow')))
        Set-Acl -LiteralPath $rdpFile -AclObject $acl
        @"
full address:s:127.0.0.1
username:s:$env:COMPUTERNAME\$Name
password 51:b:$passwordHex
screen mode id:i:1
desktopwidth:i:800
desktopheight:i:600
session bpp:i:32
authentication level:i:0
enablecredsspsupport:i:1
prompt for credentials:i:0
redirectclipboard:i:0
redirectprinters:i:0
audiomode:i:2
"@ | Set-Content -LiteralPath $rdpFile -Encoding Unicode
        $client = Start-Process -FilePath "$env:SystemRoot\System32\mstsc.exe" -ArgumentList ('"' + $rdpFile + '"') -PassThru
        $deadline = (Get-Date).AddSeconds(60)
        do {
            $sessionId = [QuesmaTestSessions]::Find($Name)
            if ($sessionId -gt 0) { return [pscustomobject]@{ Client = $client; RdpFile = $rdpFile; Id = $sessionId } }
            Start-Sleep -Milliseconds 100
        } while ((Get-Date) -lt $deadline)
        throw "No desktop session for the test user; mstsc exited=$($client.HasExited)"
    } catch {
        $bounds = [Windows.Forms.Screen]::PrimaryScreen.Bounds
        $bitmap = New-Object Drawing.Bitmap($bounds.Width, $bounds.Height)
        $graphics = [Drawing.Graphics]::FromImage($bitmap)
        try {
            $graphics.CopyFromScreen($bounds.Location, [Drawing.Point]::Empty, $bounds.Size)
            $diagnosticDir = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }
            $bitmap.Save((Join-Path $diagnosticDir 'quesma-rdp.png'))
        } finally { $graphics.Dispose(); $bitmap.Dispose() }
        Get-NetTCPConnection -LocalPort 3389 -ErrorAction SilentlyContinue | Format-Table | Out-String | Write-Host
        & "$env:SystemRoot\System32\query.exe" user | Out-String | Write-Host
        foreach ($log in @('Microsoft-Windows-TerminalServices-LocalSessionManager/Operational',
            'Microsoft-Windows-TerminalServices-RemoteConnectionManager/Operational',
            'Microsoft-Windows-RemoteDesktopServices-RdpCoreTS/Operational')) {
            Get-WinEvent -LogName $log -MaxEvents 5 -ErrorAction SilentlyContinue |
                Select-Object TimeCreated, Id, Message | Format-List | Out-String | Write-Host
        }
        Get-WinEvent -FilterHashtable @{ LogName = 'System'; StartTime = (Get-Date).AddMinutes(-3); Level = 1, 2, 3 } `
            -MaxEvents 10 -ErrorAction SilentlyContinue |
            Select-Object TimeCreated, Id, Message | Format-List | Out-String | Write-Host
        Get-WinEvent -FilterHashtable @{ LogName = 'Security'; Id = 4625; StartTime = (Get-Date).AddMinutes(-3) } `
            -MaxEvents 5 -ErrorAction SilentlyContinue |
            Select-Object TimeCreated, Id, Message | Format-List | Out-String | Write-Host
        Get-Process csrss, winlogon, dwm, mstsc -ErrorAction SilentlyContinue |
            Select-Object Name, Id, SessionId | Format-Table | Out-String | Write-Host
        if ($client -and -not $client.HasExited) { $client.Kill() }
        Remove-Item -LiteralPath $rdpFile -Force -ErrorAction SilentlyContinue
        throw
    }
}

function Close-TestSession($Session) {
    if (-not $Session) { return }
    try { [QuesmaTestSessions]::Logoff($Session.Id) }
    finally {
        if (-not $Session.Client.HasExited) { $Session.Client.Kill() }
        Remove-Item -LiteralPath $Session.RdpFile -Force -ErrorAction SilentlyContinue
    }
}
