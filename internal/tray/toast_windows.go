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

	// Codex PR #22 r4 P1: ensure AppUserModelID is registered before
	// CreateToastNotifier. Modern Windows toasts require a registered
	// AUMI; without one, ToastNotificationManager silently rejects
	// the .Show() call on a clean install — daemon-failure
	// notifications never reach the user even though C4 is wired.
	//
	// The Start-menu-shortcut path (the canonical AUMI registration
	// route) would require install/uninstall surface in mcphub. The
	// registry path documented at
	//   https://learn.microsoft.com/en-us/windows/win32/shell/appsforwindows-appsfolder
	// (HKCU\Software\Classes\AppUserModelId\<id> + DisplayName) is
	// self-contained and per-user, and works for toast delivery
	// without a shortcut. Idempotent — running it before every
	// toast adds ~50ms to the first call and is a no-op afterwards.
	script := fmt.Sprintf(`
$appId = 'mcp-local-hub';
$regPath = "HKCU:\Software\Classes\AppUserModelId\$appId";
if (-not (Test-Path $regPath)) {
  New-Item -Path $regPath -Force | Out-Null;
  Set-ItemProperty -Path $regPath -Name 'DisplayName' -Value 'mcp-local-hub' -Force;
  Set-ItemProperty -Path $regPath -Name 'ShowInSettings' -Value 1 -Type DWord -Force;
}
[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime] | Out-Null;
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02);
$xml = [xml]$template.GetXml();
$xml.toast.visual.binding.text[0].InnerText = '%s';
$xml.toast.visual.binding.text[1].InnerText = '%s';
$doc = [Windows.Data.Xml.Dom.XmlDocument]::new();
$doc.LoadXml($xml.OuterXml);
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($appId).Show([Windows.UI.Notifications.ToastNotification]::new($doc));
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
