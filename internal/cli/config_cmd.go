package cli

import (
	"github.com/spf13/cobra"
)

// newConfigCmdReal builds the `mcphub config` parent command. It groups
// configuration-management subcommands such as overlay-quarantine
// (Task 2.6) and prune-orphan-overlay-rows (Task 5.1); those children
// are wired by their respective tasks. This task only creates the
// empty parent so cobra prints help when invoked without a subcommand.
func newConfigCmdReal() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Configuration-management subcommands (overlay quarantine, orphan pruning, etc.)",
		Long: `Groups configuration-management subcommands.

Subcommands manage operator-facing configuration state — overlay
quarantine, orphan-row pruning, and similar maintenance operations
— without restarting daemons or rewriting manifests.

Run 'mcphub config --help' to list available subcommands.`,
	}
	// Task 2.6: offline overlay-quarantine recovery command.
	c.AddCommand(newOverlayQuarantineCmd())
	return c
}
