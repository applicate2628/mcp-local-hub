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
// machine-state dispatcher that picks the right sink based on what is
// on disk:
//
//   - v0.5.x host (supervisor-intent.json with ≥1 daemon row) →
//     the rename-aside + IPC-handoff cold-restart upgrade.
//   - v0.4 scheduler-only host (legacy scheduler daemon tasks, no
//     supervisor intent) → the legacy-scheduler upgrade migration.
//   - fresh install (no supervisor state on disk) → the legacy
//     binary-copy body (stop every mcp-local-hub-* daemon, copy the
//     running binary over ~/.local/bin/mcphub.exe bootstrap copy-only
//     skipping PATH registration per bot r2 P1 on PR #181, restart
//     daemons).
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
		Long: `Stop every mcp-local-hub-* daemon, copy the currently-running mcphub
binary over the canonical path at ~/.local/bin/mcphub.exe, then restart
every daemon from the new binary. The operation is idempotent on the copy
step — re-running when the canonical binary is already current is a no-op.

What upgrade does:
  1. Self-replace guard. Refuses when run from the canonical path
     (the running image cannot replace itself on Windows). Build
     a new binary (e.g., ` + "`go build ./cmd/mcphub`" + `) and run
     ` + "`./mcphub upgrade`" + ` from the build directory.
  2. StopAll. Kills every mcp-local-hub-* daemon by port and /Ends
     its scheduler task — releases the Windows file lock on the
     canonical path. Per-task stop failures are logged but non-fatal.
  3. Copy-only bootstrap. Tempfile + atomic rename of os.Executable()
     to the canonical path. PATH registration is intentionally
     skipped (that's ` + "`mcphub setup`" + `'s job) so a HKCU PATH write
     hiccup mid-upgrade doesn't leave the fleet down.
  4. RestartAll. /Run every paused task. New tasks reference the
     canonical path so they pick up the new binary automatically.

Examples:
  go build ./cmd/mcphub                     # in a checkout
  ./mcphub upgrade                          # apply this build to ~/.local/bin/

  # Equivalent (older form):
  ./mcphub install --upgrade

This subcommand is a discoverability alias for ` + "`install --upgrade`" + `
introduced in bug-bash A7. Both forms route through the same
machine-state dispatcher (dispatchUpgrade), so they run the same code
path: a v0.5+ host takes the supervisor rename-aside + IPC-handoff
cold-restart, a v0.4 scheduler-only host takes the legacy-scheduler
upgrade migration, and a fresh install falls back to the
binary-copy + restart body described above.

Limitations:
  - The GUI/tray process is NOT auto-stopped by upgrade (gap tracked
    as bug-bash A8 for v0.4.1). If your GUI is running it will hold a
    file lock on the canonical .exe and Bootstrap will fail with
    "Access is denied". Workaround: close the tray icon (or kill the
    mcphub gui process) before running upgrade. After upgrade,
    restart the GUI manually.
  - Watchdog tick (5-min cadence) running concurrent with upgrade
    is rare (~1s upgrade window) but theoretically can revive a
    daemon mid-copy and re-lock the binary. If it happens, you'll
    see a "target in use" error and can re-run upgrade.

See also: install, setup, restart, stop, watchdog.`,
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
