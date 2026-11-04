package windows

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func NotifyRevocation(ctx context.Context, message string) bool {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return false
	}
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil || session == 0 {
		return false
	}
	script := `$ErrorActionPreference = 'Stop'; [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; [Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null; $xml = New-Object Windows.Data.Xml.Dom.XmlDocument; $xml.LoadXml('<toast><visual><binding template="ToastText02"><text id="1">Quesma Shipper: install revoked</text><text id="2"></text></binding></visual></toast>'); $xml.GetElementsByTagName('text').Item(1).InnerText = '` + strings.ReplaceAll(message, "'", "''") + `'; $toast = [Windows.UI.Notifications.ToastNotification]::new($xml); $notifier = [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('com.quesma.shipper'); if ($notifier.Setting -ne 0) { exit 1 }; $notifier.Show($toast)`
	return exec.CommandContext(ctx, filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-Command", script).Run() == nil
}
