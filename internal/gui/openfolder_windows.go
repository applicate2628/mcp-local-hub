// internal/gui/openfolder_windows.go
//go:build windows

package gui

func openFolderImpl(path string) error {
	// `explorer.exe /select,<path>` opens the parent dir AND
	// highlights the file. The leading commas-no-space form is
	// Microsoft's quirk; do NOT add a space.
	_ = openFolderSpawn("explorer.exe", "/select,"+path)
	return nil // fire-and-forget per Codex r5 #3
}
