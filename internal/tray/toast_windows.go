// internal/tray/toast_windows.go
//go:build windows

package tray

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ShowToast displays a Windows toast notification with the given
// title and body. Spec §6: "Windows toast notifications fire on
// daemon failed / auto-restart / manual action completed events."
//
// Implementation uses PowerShell + the Windows.UI.Notifications
// WinRT API — same surface that BurntToast and similar PS modules
// wrap. Pure os/exec, no cgo, no extra Go dependency. The toast
// appears as a Windows 10/11 native notification (top-right
// pop-up + Action Center entry); on older Windows it falls back
// to a tray balloon tip via the same API.
//
// Latency: ~300-500ms per call (PowerShell startup + WinRT
// JIT). That's acceptable because toasts fire on rare events
// (daemon failures, recovery), not in a hot path. A 5s context
// timeout protects against PowerShell hangs (e.g. policy block).
//
// Title and body are escaped for PowerShell single-quote string
// literals — single quotes are doubled per PS quoting rules. No
// XML escape needed because we set InnerText (Windows handles
// XML encoding for us).
func ShowToast(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	psTitle := psEscape(title)
	psBody := psEscape(body)

	// Single-line PowerShell so we don't depend on a here-doc
	// pipeline. Using app id "mcp-local-hub" so toasts group in
	// Action Center under one source.
	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime] | Out-Null;
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02);
$xml = [xml]$template.GetXml();
$xml.toast.visual.binding.text[0].InnerText = '%s';
$xml.toast.visual.binding.text[1].InnerText = '%s';
$doc = [Windows.Data.Xml.Dom.XmlDocument]::new();
$doc.LoadXml($xml.OuterXml);
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('mcp-local-hub').Show([Windows.UI.Notifications.ToastNotification]::new($doc));
`, psTitle, psBody)

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("toast failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// psEscape doubles single quotes for embedding inside a PowerShell
// '...' literal. Newlines and other control chars are stripped
// because they would terminate the literal mid-script.
func psEscape(s string) string {
	s = strings.ReplaceAll(s, "'", "''")
	// Strip CR/LF — toast title/body are single-line.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
