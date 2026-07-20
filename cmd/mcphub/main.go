// Build metadata injection happens through ldflags at link time — see
// build.ps1 / build.sh in repo root. Binary version info for Windows
// Explorer Properties is embedded via cmd/mcphub/resource.syso, regenerated
// from versioninfo.json whenever the file changes:
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0 -64 -o resource.syso versioninfo.json
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

	// A bare invocation (no subcommand) is the first-run entry point: route
	// it to `gui` so `mcphub` starts the hub + GUI. See shouldAutoLaunchGUI.
	if shouldAutoLaunchGUI() {
		os.Args = append(os.Args, "gui")
	}

	if err := cli.NewRootCmd().Execute(); err != nil {
		// PR #23 C1 stuck-instance recovery: propagate distinct exit
		// codes from --force / --force --kill (2/3/4/6/7) instead of
		// cobra's default "1 on error". cli.forceExitError implements
		// the combined interface below; errors.As keeps this main.go
		// branch agnostic to the concrete unexported type.
		//
		// Codex iter-5 P2: the matcher MUST require IsMcphubForceExit
		// in addition to ExitCode. *os/exec.ExitError also satisfies
		// `interface{ ExitCode() int }`, so a bare ExitCode-only
		// match would silently route wrapped subprocess failures
		// (e.g. `mcphub manifest edit` / `mcphub secrets edit` editor
		// fmt.Errorf("...: %w", err) wrappings) to os.Exit(<child>),
		// hiding the contextual "error: ..." diagnostic. The marker
		// method is unique to cli.forceExitError; *exec.ExitError
		// does not implement it.
		var fe interface {
			ExitCode() int
			IsMcphubForceExit() bool
		}
		if errors.As(err, &fe) {
			os.Exit(fe.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// shouldAutoLaunchGUI reports whether this invocation carried no subcommand
// and should therefore be routed to `gui`.
//
// Contract: a BARE `mcphub` launches the hub + GUI, regardless of whether a
// console is attached. Both entry points the operator actually uses converge
// here — an Explorer double-click (no console) and `mcphub` typed at a
// terminal (console attached) — so "install it, run it, it works" holds in
// both. The pre-2026-07 behavior additionally required "no console attached",
// which made a terminal `mcphub` dump the full command list instead.
//
// Anything with at least one argument is untouched: `mcphub --help`,
// `mcphub help`, and every subcommand keep their existing behavior because
// they never reach this branch.
func shouldAutoLaunchGUI() bool {
	return shouldAutoLaunchGUIForArgs(os.Args)
}

// shouldAutoLaunchGUIForArgs is the pure, testable core of
// shouldAutoLaunchGUI. args follows the os.Args convention: args[0] is the
// program path, so a bare invocation has length 1. A zero-length slice is
// treated as bare (defensive; the Go runtime always supplies argv[0]).
func shouldAutoLaunchGUIForArgs(args []string) bool {
	return len(args) <= 1
}
