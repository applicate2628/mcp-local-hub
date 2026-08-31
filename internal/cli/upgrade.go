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
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Transactionally apply an admitted build to the canonical mcphub binary (alias for `install --upgrade`)",
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
  - The GUI/tray process is not auto-stopped. A running GUI that owns the
    canonical image lock is rejected by preflight before fleet mutation;
    close it and rerun upgrade.
See also: install, setup, restart, stop.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The alias owns no flags; the shared dispatcher owns all policy.
			return dispatchUpgrade(cmd)
		},
	}
}
