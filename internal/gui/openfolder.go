// internal/gui/openfolder.go
package gui

import (
	"os/exec"

	"mcp-local-hub/internal/process"
)

// openFolderSpawn is the injectable seam used by OpenFolderAt.
// Tests overwrite it; production callers use exec.Command(...).Start()
// via openFolderDefault below.
var openFolderSpawn = openFolderDefault

// openFolderDefault wraps exec.Command + Start. NoConsole keeps the
// explorer / xdg-open / open invocation from flashing a console
// window when called from a windowsgui-subsystem parent.
func openFolderDefault(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	process.NoConsole(cmd)
	return cmd.Start()
}

// OpenFolderAt opens the file manager focused on the given file's
// parent directory (and selects the file on Windows where the
// shell exposes that capability). Best-effort fire-and-forget per
// Codex r5 #3: if the spawn fails, the diagnostic flow has already
// printed the path to stdout so the operator can navigate manually.
//
// Cross-platform dispatch lives in openfolder_windows.go and
// openfolder_other.go; this function is just the public entry that
// tests hook through openFolderSpawn.
func OpenFolderAt(path string) error {
	return openFolderImpl(path)
}
