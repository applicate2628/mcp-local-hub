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
	"strings"

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
	args := os.Args
	consolePolicy, args := cli.ResolveWindowsConsolePolicy(args)
	debugConsoleAcquired, err := applyWindowsConsolePolicy(consolePolicy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cli.SetBuildInfo(version, commit, buildDate)
	cli.SetDebugConsoleAcquired(debugConsoleAcquired)

	// A bare invocation (no subcommand) is the first-run entry point: route
	// it to `gui` so `mcphub` starts the hub + GUI. See shouldAutoLaunchGUI.
	if !autoLaunchGUIOptedOut() && shouldAutoLaunchGUIForArgs(args) {
		args = cli.RouteInvocationArgs(args)
	}
	os.Args = args

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
//
// OPT-OUT: MCPHUB_NO_AUTO_GUI=1 restores the pre-2026-07 behavior for a
// bare `mcphub` (print the command list, exit 0). This exists because the
// routing change above is a CONTRACT change on the bare invocation — it
// went from "print help, exit 0" to "bind a port, spawn a supervisor,
// block forever" — and anything that ran bare `mcphub` as a cheap liveness
// or smoke check (CI step, healthcheck, packaging test, a bare `mcphub`
// over ssh) would otherwise hang or, if a GUI is already up, exit 1 on the
// single-instance lock. The env var is the escape hatch for those callers;
// it is deliberately checked HERE and not in the pure seam below.
func shouldAutoLaunchGUI() bool {
	if autoLaunchGUIOptedOut() {
		return false
	}
	return shouldAutoLaunchGUIForArgs(os.Args)
}

// NoAutoGUIEnv opts a bare `mcphub` out of the auto-GUI route.
const NoAutoGUIEnv = "MCPHUB_NO_AUTO_GUI"

// autoLaunchGUIOptedOut isolates the ambient-environment read so
// shouldAutoLaunchGUIForArgs stays pure and table-testable. Truthy
// parsing mirrors the repo's other env knobs ("1" or "true" after
// trim+lowercase).
func autoLaunchGUIOptedOut() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(NoAutoGUIEnv)))
	return v == "1" || v == "true"
}

// shouldAutoLaunchGUIForArgs is the pure, testable core of
// shouldAutoLaunchGUI. args follows the os.Args convention: args[0] is the
// program path, so a bare invocation has length 1. A zero-length slice is
// treated as bare (defensive; the Go runtime always supplies argv[0]).
func shouldAutoLaunchGUIForArgs(args []string) bool {
	return len(args) <= 1
}
