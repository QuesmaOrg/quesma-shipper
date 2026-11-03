package packaging

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
)

// NotifyRevocation is best effort: managed services may have no interactive desktop.
func NotifyRevocation(ctx context.Context, message string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", `on run argv
 display notification (item 1 of argv) with title "Quesma Shipper: install revoked"
end run`, message).Run()
	case "windows":
		script := `$ErrorActionPreference = 'Stop'; [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; [Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null; $xml = New-Object Windows.Data.Xml.Dom.XmlDocument; $xml.LoadXml('<toast><visual><binding template="ToastText02"><text id="1">Quesma Shipper: install revoked</text><text id="2"></text></binding></visual></toast>'); $xml.GetElementsByTagName('text').Item(1).InnerText = '` + strings.ReplaceAll(message, "'", "''") + `'; $toast = [Windows.UI.Notifications.ToastNotification]::new($xml); [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Quesma Shipper').Show($toast)`
		_ = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Run()
	}
}
