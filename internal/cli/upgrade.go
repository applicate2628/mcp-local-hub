package cli

import (
	"fmt"

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
// Both spellings share admission, preflight, rollback, readiness, and receipt.
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
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Transactionally apply an admitted Product to canonical mcphub.exe (alias for `install --upgrade`)",
		Long: `Apply an admitted mcphub product build through the managed supervisor
upgrade transaction. A daemon-bearing supervisor intent must already exist.

What upgrade does:
  1. Self-replace guard. Refuses when run from the canonical path
     (the running image cannot replace itself on Windows). Build
	     a new Windows product binary with ` + "`pwsh ./build.ps1`" + ` and run
	     ` + "`./bin/mcphub.exe upgrade`" + ` from the build directory.
  2. Stage and admit the one Windows Product role from its strict embedded
     VERSIONINFO identity and SHA-256 before touching the running fleet.
  3. Quiesce and exit/reap the prior supervisor; prove its lock and every
     expected daemon port are released; re-admit the unchanged candidate.
  4. Promote and read back canonical mcphub.exe, rewrite exact-owned tasks,
     start and identity-bind the successor, retire an admitted legacy adapter,
     read back commit_decided, remove only journal-bound executable residue,
     then write/read back upgrade-receipt-v3 before publishing its event.
  5. Any pre-commit_decided failure restores exact prior product, task,
     adapter, and receipt state and proves required prior readiness. After a
     read-back commit_decided, recovery completes only that identity forward.

Examples:
  pwsh ./build.ps1                           # in a checkout
  ./bin/mcphub.exe upgrade                  # apply this admitted build
  ./bin/mcphub.exe upgrade --terminate-gui-for-upgrade # opt in to exact GUI/tray force-exit
  ./bin/mcphub.exe upgrade --terminate-gui-for-upgrade --stop-gui-owned-fleet-for-upgrade # opt in to exact GUI-owned fleet stop
  ./bin/mcphub.exe upgrade --complete-committed-recovery # only finish an existing Windows V3 commit_decided transaction

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
  - By default a running GUI is refused before fleet mutation; close it and
    rerun upgrade. The positive --terminate-gui-for-upgrade option is Windows
    managed-upgrade only. It force-exits a verified GUI/tray generation under
    one upgrade fence and may lose unsaved GUI-only work. Pending recovery or
    uncertain process, task, or lease identity still refuses before exit.
  - --complete-committed-recovery is exclusive, Windows-only, and uses an
    external admitted Product solely to finish an identity-bound committed
    transaction. It never stages or promotes that external executable.
See also: install, setup, restart, stop.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			recovery, _ := cmd.Flags().GetBool("complete-committed-recovery")
			if recovery {
				if len(args) != 0 || upgradeGUIRequested(cmd) || upgradeGUIOwnedFleetStopRequested(cmd) {
					return fmt.Errorf("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: recovery takes no arguments or GUI-stop flags")
				}
				if committedRecoveryFn == nil {
					return fmt.Errorf("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: committed Product recovery is unsupported on this platform")
				}
				return committedRecoveryFn(cmd)
			}
			if upgradeGUIOwnedFleetStopRequested(cmd) && !upgradeGUIRequested(cmd) {
				return fmt.Errorf("--stop-gui-owned-fleet-for-upgrade requires --terminate-gui-for-upgrade")
			}
			// The alias owns no flags; the shared dispatcher owns all policy.
			return dispatchUpgrade(cmd)
		},
	}
	c.Flags().Bool("terminate-gui-for-upgrade", false,
		"after a held upgrade fence and exact identity checks, force-exit the incumbent GUI/tray; this may lose unsaved GUI-only work")
	c.Flags().Bool("stop-gui-owned-fleet-for-upgrade", false,
		"with --terminate-gui-for-upgrade: permit a verified GUI-child Job-bound supervisor fleet stop after V3 recovery preimage readback")
	c.Flags().Bool("complete-committed-recovery", false,
		"complete only an existing, identity-verified Windows V3 commit_decided Product upgrade; never stage or promote this executable")
	return c
}

// Windows installs the sole Product-owned forward-recovery adapter. Other
// platforms leave this nil and reject the opt-in without side effects.
var committedRecoveryFn func(*cobra.Command) error
