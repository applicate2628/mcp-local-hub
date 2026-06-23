// internal/gui/openfolder.go
package gui

import (
	"os/exec"

	"mcp-local-hub/internal/process"
)

// openFolderSpawn is the injectable seam used by openFolderImpl.
// Tests overwrite it; production callers use exec.Command(...).Start()
// via openFolderDefault below.
var openFolderSpawn = openFolderDefault

// openFolderDefault wraps exec.Command + Start. NoConsole keeps the
// explorer / xdg-open / open invocation from flashing a console
// window when called from a windowsgui-subsystem parent. Fire-and-forget:
// the launcher is never Wait()ed on, and the surviving caller (the
// one-shot `mcphub gui --force --reveal` CLI process) exits right after,
// so its handles are freed by process teardown.
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
// The sole production caller is the one-shot `mcphub gui --force --reveal`
// CLI process; bare `--force` is print-only and never reaches here. There
// is no reveal-window reaper: under the Windows "Launch folder windows in
// a separate process" (SeparateProcess=1) setting each /select spawns a
// SEPARATE persistent explorer.exe that hands off and outlives the
// launcher, so the launcher PID is dead within seconds and nothing the
// hub tracks ever observes the real window. --reveal therefore accepts
// ONE un-reapable persistent window per invocation.
//
// Cross-platform dispatch lives in openfolder_windows.go and
// openfolder_other.go; this function is just the public entry that
// tests hook through openFolderSpawn.
func OpenFolderAt(path string) error {
	return openFolderImpl(path)
}
