// Build metadata injection happens through ldflags at link time — see
// build.ps1 / build.sh in repo root. Binary version info for Windows
// Explorer Properties is embedded via cmd/mcphub/resource.syso, regenerated
// from versioninfo.json whenever the file changes:
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -64 -o resource.syso versioninfo.json
package main

import (
	"errors"
	"fmt"
	"os"

	"mcp-local-hub/internal/cli"
)

// These are populated at build time via `-ldflags "-X ..."` (see build
// scripts). Defaults are useful for `go run` / unmarked dev builds.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	attachParentConsoleIfAvailable()
	cli.SetBuildInfo(version, commit, buildDate)

	// Explorer double-click: no args and no parent console ⇒ auto-launch GUI.
	// (Detect by checking whether os.Args has any command and stdout is a
	// pipe/console. If neither, route to `gui`.)
	if shouldAutoLaunchGUI() {
		os.Args = append(os.Args, "gui")
	}

	if err := cli.NewRootCmd().Execute(); err != nil {
		// PR #23 C1 stuck-instance recovery: propagate distinct exit
		// codes from --force / --force --kill (2/3/4/6/7) instead of
		// cobra's default "1 on error". cli.forceExitError implements
		// `interface{ ExitCode() int }`; errors.As keeps this main.go
		// branch agnostic to the concrete unexported type.
		var fe interface{ ExitCode() int }
		if errors.As(err, &fe) {
			os.Exit(fe.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// shouldAutoLaunchGUI returns true when we were started with no command-line
// arguments AND we have no console attached — the hallmark of an Explorer
// double-click on a Windows-subsystem binary.
func shouldAutoLaunchGUI() bool {
	if len(os.Args) > 1 {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		// Invalid handle (typical for GUI subsystem with no parent console,
		// no redirect) → launch GUI.
		return true
	}
	// If stdout is a character device (console), we're in a shell — don't
	// auto-launch GUI; let cobra's default help print normally.
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return false
	}
	// stdout is a regular file or pipe — user redirected output, so don't
	// launch GUI; let cobra's default help print to the redirect target.
	return false
}
