package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd builds the top-level `mcphub` command with all subcommand stubs attached.
// Subcommand implementations are filled in by later tasks; this task only wires the tree.
//
// Named "mcphub" (rather than "mcp") to avoid the name collision with the
// Python mcp SDK which installs a binary of the same name via pip.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mcphub",
		Short:         "Local shared-daemon manager for MCP servers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newInstallCmd())
	root.AddCommand(newUninstallCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newScanCmd())
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newRelayCmd())
	root.AddCommand(newSecretsCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newCleanupCmd())
	return root
}

// Stub constructors — each returns a cobra.Command that prints "not implemented yet".
// Later tasks replace each RunE with real logic.
func newInstallCmd() *cobra.Command {
	return newInstallCmdReal()
}
func newUninstallCmd() *cobra.Command { return newUninstallCmdReal() }
func newStatusCmd() *cobra.Command  { return newStatusCmdReal() }
func newScanCmd() *cobra.Command    { return newScanCmdReal() }
func newMigrateCmd() *cobra.Command { return newMigrateCmdReal() }
func newRestartCmd() *cobra.Command { return newRestartCmdReal() }
func newRollbackCmd() *cobra.Command { return newRollbackCmdReal() }
func newDaemonCmd() *cobra.Command  { return newDaemonCmdReal() }
func newRelayCmd() *cobra.Command   { return newRelayCmdReal() }
func newVersionCmd() *cobra.Command { return newVersionCmdReal() }
func newSecretsCmd() *cobra.Command {
	return newSecretsCmdReal()
}
func newLogsCmd() *cobra.Command {
	return newLogsCmdReal()
}
func newCleanupCmd() *cobra.Command {
	return newCleanupCmdReal()
}
func newStopCmd() *cobra.Command {
	return newStopCmdReal()
}

func stub(name string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cmd.Printf("mcp %s: not implemented yet\n", name)
		return nil
	}
}
