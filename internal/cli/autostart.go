// Package cli — `mcphub autostart {enable|disable|status}` cobra
// command (plan §2531-2541 Task 11.1).
//
// The CLI is a thin shell over `internal/autostart.Backend`. It owns:
//
//   - Flag wiring (--strict-mode threads into Options.StrictMode).
//   - Output formatting (Status prints the state String() verbatim
//     so scripts can grep for "absent" / "enabled-running" / …).
//   - Backend factory swap for tests (autostartBackendFactoryFn).
//
// All OS-specific logic — Task Scheduler XML, systemctl unit, launchctl
// plist — lives in the autostart package, not here. This file is
// portable across all three target OSes.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/autostart"
)

// autostartBackendFactoryFn is the test seam — production paths leave
// it pointing at autostart.New, but autostart_test.go injects a
// recording fake. Restored via t.Cleanup between tests.
var autostartBackendFactoryFn = autostart.New

// newAutostartCmd returns the parent `mcphub autostart` cobra command
// with three subcommands attached. Registered in root.go next to
// newSuperviseCmd() so operators discover it via `mcphub --help`.
func newAutostartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autostart {enable|disable|status}",
		Short: "Manage supervisor autostart at logon",
		Long: `mcphub autostart installs (or removes) an OS-native shim that
re-runs ` + "`mcphub gui [--strict-mode]`" + ` whenever the current user
signs in.

  - Windows: Task Scheduler entry  \mcp-local-hub-supervisor
  - Linux:   systemd-user unit     ~/.config/systemd/user/mcphub-supervisor.service
  - macOS:   LaunchAgent plist     ~/Library/LaunchAgents/com.applicate2628.mcphub-supervisor.plist

` + "`status`" + ` prints one of: absent, enabled-running, enabled-stopped, drifted,
stale-residue. Drifted means the on-disk shim's args or binary path
disagree with what ` + "`mcphub autostart enable [--strict-mode]`" + ` would
write today; re-run ` + "`mcphub autostart enable`" + ` to reconcile.`,
	}
	cmd.AddCommand(newAutostartEnableCmd())
	cmd.AddCommand(newAutostartDisableCmd())
	cmd.AddCommand(newAutostartStatusCmd())
	return cmd
}

// newAutostartEnableCmd returns the `enable` subcommand.
//
// --strict-mode threads through autostart.Options into the per-OS
// shim's argv, so the supervisor process started by the shim sees the
// same `--strict-mode` flag mcphub would pass during a foreground
// `mcphub supervise` run.
func newAutostartEnableCmd() *cobra.Command {
	var strictMode bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Install or replace the autostart shim",
		Long: `Installs (or replaces) the autostart shim for the current OS. The
shim re-runs ` + "`mcphub gui [--strict-mode]`" + ` at each user logon.

Idempotent — re-running with the same flags is safe and re-writes the
on-disk shim verbatim. Re-running with different flags overwrites the
prior shim so a stale ` + "`--strict-mode`" + ` flag never lingers.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := autostartBackendFactoryFn()
			if err != nil {
				return fmt.Errorf("autostart backend: %w", err)
			}
			return b.Enable(autostart.Options{StrictMode: strictMode})
		},
	}
	cmd.Flags().BoolVar(&strictMode, "strict-mode", false,
		"pass --strict-mode through to the supervisor process the shim launches")
	return cmd
}

// newAutostartDisableCmd returns the `disable` subcommand. Disable
// takes no flags — the shim is identified by its canonical per-OS
// name, no options needed.
func newAutostartDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Remove the autostart shim",
		Long: `Removes the autostart shim for the current OS. Idempotent — runs to
completion even when no shim is currently installed.

The supervisor process is stopped as a side effect of removing the
shim's lifecycle primitive (Task Scheduler /End, systemctl --user
disable --now, launchctl bootout). If the supervisor is already
stopped, disable still succeeds.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := autostartBackendFactoryFn()
			if err != nil {
				return fmt.Errorf("autostart backend: %w", err)
			}
			return b.Disable()
		},
	}
}

// newAutostartStatusCmd returns the `status` subcommand. The output is
// the State.String() value verbatim followed by a newline — scripts
// can grep for "absent" / "enabled-running" / "enabled-stopped" /
// "drifted" / "stale-residue".
//
// --strict-mode threads through Options.StrictMode for drift
// detection: when the on-disk shim has --strict-mode but the operator
// asks `mcphub autostart status` without it, that IS drift (and vice
// versa).
func newAutostartStatusCmd() *cobra.Command {
	var strictMode bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the current autostart shim state",
		Long: `Prints one of:

  absent           — no shim installed
  enabled-running  — shim installed AND supervisor process is alive
  enabled-stopped  — shim installed but supervisor is not currently running
  drifted          — shim installed but on-disk args/path disagree with
                     what ` + "`mcphub autostart enable [--strict-mode]`" + ` would
                     write today; re-run enable to reconcile
  stale-residue    — leftover entries from a prior install (reserved)

Pass --strict-mode to check drift against the strict-mode shim shape.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := autostartBackendFactoryFn()
			if err != nil {
				return fmt.Errorf("autostart backend: %w", err)
			}
			state, err := b.Status(autostart.Options{StrictMode: strictMode})
			if err != nil {
				return fmt.Errorf("autostart status: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), state.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&strictMode, "strict-mode", false,
		"check drift against the strict-mode shim shape")
	return cmd
}
