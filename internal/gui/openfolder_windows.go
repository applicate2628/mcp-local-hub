// internal/gui/openfolder_windows.go
//go:build windows

package gui

// openFolderImpl spawns `explorer.exe /select,<path>` to open the parent
// dir AND highlight the file. The leading commas-no-space form is
// Microsoft's quirk; do NOT add a space.
//
// Returns nil regardless of spawn outcome — fire-and-forget per Codex
// r5 #3.
func openFolderImpl(path string) error {
	_ = openFolderSpawn("explorer.exe", "/select,"+path)
	return nil // fire-and-forget per Codex r5 #3
}
