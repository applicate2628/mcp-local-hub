// internal/gui/browser.go
package gui

import (
	"os/exec"
	"runtime"

	"mcp-local-hub/internal/process"
)

// spawnProcess is the injectable seam for LaunchBrowser tests.
// Uses process.NoConsole so the OS-default browser launcher (rundll32
// on Windows, etc.) does not flash a console window when invoked from
// a windowsgui-subsystem parent.
var spawnProcess = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	process.NoConsole(cmd)
	return cmd.Start()
}

// LaunchBrowser opens the GUI URL in the user's browser. Preference:
// Chrome --app -> Chromium --app -> Edge --app -> OS default. App-mode
// (--app=...) gives a chromeless window that feels more desktop-native
// than a new tab.
//
// All errors up to the last fallback are swallowed — the user can always
// open the URL manually; a failed browser launch should not fail
// `mcphub gui`.
func LaunchBrowser(url string) error {
	chromeArg := "--app=" + url
	for _, cmd := range []string{"chrome", "google-chrome", "chromium"} {
		if err := spawnProcess(cmd, chromeArg); err == nil {
			return nil
		}
	}
	if err := spawnProcess("msedge", chromeArg); err == nil {
		return nil
	}
	// OS default browser fallback.
	switch runtime.GOOS {
	case "windows":
		return spawnProcess("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return spawnProcess("open", url)
	default:
		return spawnProcess("xdg-open", url)
	}
}
