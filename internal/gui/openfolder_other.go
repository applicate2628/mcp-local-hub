// internal/gui/openfolder_other.go
//go:build !windows

package gui

import (
	"path/filepath"
	"runtime"
)

func openFolderImpl(path string) error {
	dir := filepath.Dir(path)
	if runtime.GOOS == "darwin" {
		// macOS: `open -R <path>` reveals the file in Finder.
		_ = openFolderSpawn("open", "-R", path)
	} else {
		// Linux/BSD: xdg-open opens the directory in the user's
		// configured file manager. Cannot select a file with
		// xdg-open; opening the dir is the best we can do without
		// depending on a specific desktop environment's file
		// manager (gio open / nautilus / dolphin / nemo all have
		// different selection flags).
		_ = openFolderSpawn("xdg-open", dir)
	}
	return nil // fire-and-forget
}
