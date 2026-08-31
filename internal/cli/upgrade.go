package cli

import (
	"github.com/spf13/cobra"
)

// newUpgradeCmdReal is a top-level discoverability alias for the
// `mcphub install --upgrade` flag introduced in bug-bash A7 (PR #181 /
// commit 49483ad). Users who type `mcphub --help` looking for a way to
// refresh the canonical binary from a freshly-built one expect to see
// `upgrade` in Available Commands — the flag-on-install design hid
// that workflow behind `mcphub install --upgrade`. This alias makes
// it surface alongside `install`, `restart`, `stop`, etc.
//
// The alias routes through the SAME `dispatchUpgrade` entry the
// `mcphub install --upgrade` flag uses (install.go), so the two entry
// points produce identical behavior. dispatchUpgrade is the
// machine-state dispatcher with one mutating sink:
//
//   - v0.5.x host (supervisor-intent.json with ≥1 daemon row) →
//     the admitted, receipt-backed cold-restart transaction.
//   - legacy scheduler, fresh, or unsupported platform state → stable
//     actionable refusal before any file/process mutation.
//
// bot r33 P2 closure on PR #288: BEFORE this, the alias called
// `runInstallUpgrade` directly — the LEGACY binary-copy body — so on
// a live v0.5+ host with daemon rows it bypassed the dispatcher and
// ran the stop/copy/restart path instead of the supervisor
// rename-aside / IPC handoff, which could leave the running supervisor
// on the old binary or holding the target lock. Routing through
// dispatchUpgrade fixes the divergence so both documented entry points
// behave identically. Self-replace guard, dev-build guard,
// upgrade-specific recovery hints, and the GUI-lock preflight all
// carry over identically because every sink reuses
// runInstallUpgradePreflightGuards.
//
// Why an alias instead of a flag rename:
//
//   - Backward compatibility: callers and scripts using `mcphub install
//     --upgrade` keep working. The alias is purely additive.
//   - DX: `mcphub --help` discoverability improves without forcing the
//     user to remember that upgrade lives under install.
//   - Help text: `mcphub help upgrade` works automatically via Cobra,
//     showing this command's Long block with the same diagnostic guide
//     as `mcphub install --help`'s --upgrade flag entry.
func newUpgradeCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Replace the canonical mcphub binary with the currently-running build (alias for `install --upgrade`)",
		Long: `Apply an admitted mcphub product build through the managed supervisor
upgrade transaction. A daemon-bearing supervisor intent must already exist.

What upgrade does:
  1. Self-replace guard. Refuses when run from the canonical path
     (the running image cannot replace itself on Windows). Build
	     a new Windows product binary with ` + "`pwsh ./build.ps1`" + ` and run
	     ` + "`./bin/mcphub.exe upgrade`" + ` from the build directory.
  2. Stage and admit the Windows GUI PE, non-placeholder build metadata,
     and SHA-256 before touching the running fleet.
  3. Quiesce and exit/reap the prior supervisor; prove its lock and every
     expected daemon port are released; re-admit the unchanged candidate.
  4. Promote once through rename-aside, start and identity-bind the successor,
     verify canonical bytes, then atomically write upgrade-receipt-v1.
  5. Any post-promotion failure restores the exact retained prior bytes,
     verifies their SHA-256, and proves the prior supervisor ready.

Examples:
	  pwsh ./build.ps1                           # in a checkout
	  ./bin/mcphub.exe upgrade                  # apply this admitted build

  # Equivalent (older form):
  ./mcphub install --upgrade

This subcommand is a discoverability alias for ` + "`install --upgrade`" + `
introduced in bug-bash A7. Both forms route through the same
machine-state dispatcher (dispatchUpgrade), so they run the same code
path: a v0.5+ host takes the supervisor rename-aside + IPC-handoff
cold-restart transaction. Legacy-scheduler, fresh-install, and unsupported
platform states fail closed without mutation and print the required setup or
package-workflow recovery.

Limitations:
  - The GUI/tray process is NOT auto-stopped by upgrade (gap tracked
    as bug-bash A8 for v0.4.1). If your GUI is running it will hold a
    file lock on the canonical .exe and Bootstrap will fail with
    "Access is denied". Workaround: close the tray icon (or kill the
    mcphub gui process) before running upgrade. After upgrade,
    restart the GUI manually.
See also: install, setup, restart, stop.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// bot r33 P2 closure on PR #288: route the top-level
			// `mcphub upgrade` alias through the SAME machine-state
			// dispatcher the `install --upgrade` flag uses, NOT the
			// legacy runInstallUpgrade body directly. The alias has none
			// of install's flags (--server/--daemon/--all/...), so it
			// needs no mutual-exclusivity guard before the call;
			// dispatchUpgrade does not read those flags off cmd.
			return dispatchUpgrade(cmd)
		},
	}
}
