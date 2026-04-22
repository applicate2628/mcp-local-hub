// internal/gui/browser.go
package gui

import (
	"os/exec"
	"runtime"
)

// spawnProcess is the injectable seam for LaunchBrowser tests.
var spawnProcess = func(name string, args ...string) error {
	return exec.Command(name, args...).Start()
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
