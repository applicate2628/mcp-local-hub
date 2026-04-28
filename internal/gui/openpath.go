// internal/gui/openpath.go
//
// OpenPath opens a filesystem directory in the OS file manager. Unlike
// LaunchBrowser (which is browser-oriented and tries Chrome --app first),
// OpenPath goes straight to the file-manager:
//   - Windows: explorer.exe <path>
//   - macOS:   open <path>
//   - Linux:   xdg-open <path>
//
// Reuses the spawnProcess seam already defined in browser.go so tests
// can intercept the spawn without touching the OS file manager.
//
// Memo §2.4 (Codex r1 P1.6): originally we proposed reusing LaunchBrowser
// for the "Open app-data folder" action, but LaunchBrowser opens browsers,
// not file managers — Chrome would happily load the path in a tab. This
// dedicated helper avoids that confusion.

package gui

import "runtime"

func OpenPath(path string) error {
	switch runtime.GOOS {
	case "windows":
		return spawnProcess("explorer.exe", path)
	case "darwin":
		return spawnProcess("open", path)
	default:
		return spawnProcess("xdg-open", path)
	}
}
