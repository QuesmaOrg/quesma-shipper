# A loopback RDP connection supplies a real standard-user desktop session on disposable Windows Server runners.
Add-Type -AssemblyName System.Windows.Forms
Add-Type -ReferencedAssemblies System, System.Windows.Forms, System.Drawing -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.RegularExpressions;
public static class QuesmaTestSessions {
    delegate bool EnumWindow(IntPtr window, IntPtr parameter);
    [DllImport("user32.dll")] static extern bool EnumWindows(EnumWindow callback, IntPtr parameter);
    [DllImport("user32.dll")] static extern bool EnumChildWindows(IntPtr parent, EnumWindow callback, IntPtr parameter);
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr window, out int processId);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetWindowTextW(IntPtr window, StringBuilder text, int length);
    [DllImport("user32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr SendMessageTimeoutW(IntPtr window, uint message, IntPtr wparam, StringBuilder text, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassNameW(IntPtr window, StringBuilder text, int length);
    [DllImport("user32.dll")] static extern int GetDlgCtrlID(IntPtr window);
    [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll")] static extern bool IsWindowEnabled(IntPtr window);
    [DllImport("user32.dll", SetLastError=true)] static extern bool PostMessageW(IntPtr window, uint message, IntPtr wparam, IntPtr lparam);
    static string WindowText(IntPtr window) {
        var text = new StringBuilder(4096);
        GetWindowTextW(window, text, text.Capacity);
        return text.ToString();
    }
    static string WindowClass(IntPtr window) {
        var text = new StringBuilder(256);
        GetClassNameW(window, text, text.Capacity);
        return text.ToString();
    }
    static string ControlText(IntPtr window) {
        var text = new StringBuilder(4096);
        IntPtr result;
        if (SendMessageTimeoutW(window, 0xD, new IntPtr(text.Capacity), text, 2, 500, out result) == IntPtr.Zero) return "";
        return text.ToString();
    }
    public static string WindowDiagnostics(int processId) {
        var output = new StringBuilder();
        EnumWindows(delegate(IntPtr window, IntPtr unused) {
            int owner;
            GetWindowThreadProcessId(window, out owner);
            if (owner != processId) return true;
            output.AppendLine("Window PID=" + owner + " class=" + WindowClass(window) + " title=" + WindowText(window));
            EnumChildWindows(window, delegate(IntPtr child, IntPtr ignored) {
                string kind = WindowClass(child);
                output.AppendLine("  Child ID=" + GetDlgCtrlID(child) + " class=" + kind + " text=" + (kind == "Edit" ? "<redacted>" : ControlText(child)));
                return true;
            }, IntPtr.Zero);
            return true;
        }, IntPtr.Zero);
        return output.Length == 0 ? "No native windows found for PID=" + processId : output.ToString();
    }
    public static bool ConfirmCertificate(int processId, string computerName) {
        bool accepted = false;
        EnumWindows(delegate(IntPtr window, IntPtr unused) {
            int owner;
            GetWindowThreadProcessId(window, out owner);
            if (owner != processId || !IsWindowVisible(window) || WindowClass(window) != "#32770" ||
                WindowText(window) != "Remote Desktop Connection") return true;
            var text = new StringBuilder();
            IntPtr yes = IntPtr.Zero;
            EnumChildWindows(window, delegate(IntPtr child, IntPtr ignored) {
                string label = ControlText(child);
                text.AppendLine(label);
                int id = GetDlgCtrlID(child);
                if ((id == 6 || id == 14004) && WindowClass(child) == "Button" &&
                    label.Replace("&", "") == "Yes" && IsWindowVisible(child) && IsWindowEnabled(child)) yes = child;
                return true;
            }, IntPtr.Zero);
            string content = Regex.Replace(text.ToString(), @"\s+", " ");
            if (yes == IntPtr.Zero || !content.Contains("The remote computer could not be authenticated due to problems with its security certificate.") ||
                !content.Contains("Do you want to connect despite these certificate errors?") ||
                (content.IndexOf(computerName, StringComparison.OrdinalIgnoreCase) < 0 && !content.Contains("127.0.0.1"))) return true;
            if (!PostMessageW(yes, 0xF5, IntPtr.Zero, IntPtr.Zero)) throw new Win32Exception(Marshal.GetLastWin32Error());
            accepted = true;
            return false;
        }, IntPtr.Zero);
        return accepted;
    }
    [StructLayout(LayoutKind.Sequential)] struct Session {
        public int Id; public IntPtr Station; public int State;
    }
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSEnumerateSessionsW(IntPtr server, int reserved, int version, out IntPtr sessions, out int count);
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSQuerySessionInformationW(IntPtr server, int session, int info, out IntPtr buffer, out int bytes);
    [DllImport("wtsapi32.dll")] static extern void WTSFreeMemory(IntPtr memory);
    [DllImport("wtsapi32.dll", SetLastError=true)] static extern bool WTSLogoffSession(IntPtr server, int session, bool wait);
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool CredReadW(string target, int type, int flags, out IntPtr credential);
    [DllImport("advapi32.dll")] static extern void CredFree(IntPtr credential);
    public static bool HasCredential(string target) {
        foreach (int type in new[] { 1, 2 }) {
            IntPtr credential;
            if (CredReadW(target, type, 0, out credential)) { CredFree(credential); return true; }
            int error = Marshal.GetLastWin32Error();
            if (error != 1168) throw new Win32Exception(error);
        }
        return false;
    }
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

function Confirm-TestCertificate([int]$ClientId) {
    if ([QuesmaTestSessions]::ConfirmCertificate($ClientId, $env:COMPUTERNAME)) {
        Write-Host "Accepted the local test certificate in mstsc process $ClientId using its native dialog."
    }
}

function Open-TestSession([string]$Name, [string]$Password) {
    $credentialTarget = 'TERMSRV/127.0.0.1'
    $credentialCreated = $false
    $client = $null
    try {
        if ([QuesmaTestSessions]::HasCredential($credentialTarget)) {
            throw 'A credential already exists for the loopback RDP target.'
        }
        & "$env:SystemRoot\System32\cmdkey.exe" "/generic:$credentialTarget" "/user:$env:COMPUTERNAME\$Name" "/pass:$Password" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not create the temporary RDP credential.' }
        $credentialCreated = $true
        $client = Start-Process -FilePath "$env:SystemRoot\System32\mstsc.exe" -ArgumentList '/v:127.0.0.1 /w:800 /h:600' -PassThru
        $deadline = (Get-Date).AddSeconds(60)
        do {
            Confirm-TestCertificate $client.Id
            $sessionId = [QuesmaTestSessions]::Find($Name)
            if ($sessionId -gt 0) { return [pscustomobject]@{ Client = $client; CredentialTarget = $credentialTarget; Id = $sessionId } }
            Start-Sleep -Milliseconds 100
        } while ((Get-Date) -lt $deadline)
        throw "No desktop session for the test user; mstsc exited=$($client.HasExited)"
    } catch {
        if ($client -and -not $client.HasExited) {
            $client.Refresh()
            Write-Host "RDP client PID=$($client.Id), window=$($client.MainWindowHandle); local computer=$env:COMPUTERNAME"
            Write-Host ([QuesmaTestSessions]::WindowDiagnostics($client.Id))
        }
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
        if ($credentialCreated) { & "$env:SystemRoot\System32\cmdkey.exe" "/delete:$credentialTarget" | Out-Null }
        throw
    }
}

function Close-TestSession($Session) {
    if (-not $Session) { return }
    try { [QuesmaTestSessions]::Logoff($Session.Id) }
    finally {
        if (-not $Session.Client.HasExited) { $Session.Client.Kill() }
        & "$env:SystemRoot\System32\cmdkey.exe" "/delete:$($Session.CredentialTarget)" | Out-Null
    }
}
