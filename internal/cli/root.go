package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd builds the top-level `mcp` command with all subcommand stubs attached.
// Subcommand implementations are filled in by later tasks; this task only wires the tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mcp",
		Short:         "Local shared-daemon manager for MCP servers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newInstallCmd())
	root.AddCommand(newUninstallCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newSecretsCmd())
	return root
}

// Stub constructors — each returns a cobra.Command that prints "not implemented yet".
// Later tasks replace each RunE with real logic.
func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		RunE:  stub("install"),
	}
}
func newUninstallCmd() *cobra.Command {
	return &cobra.Command{Use: "uninstall", Short: "Uninstall server(s)", RunE: stub("uninstall")}
}
func newStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show daemon status", RunE: stub("status")}
}
func newRestartCmd() *cobra.Command {
	return &cobra.Command{Use: "restart", Short: "Restart daemon(s)", RunE: stub("restart")}
}
func newRollbackCmd() *cobra.Command {
	return &cobra.Command{Use: "rollback", Short: "Restore pre-install client configs", RunE: stub("rollback")}
}
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{Use: "daemon", Short: "Run a daemon (invoked by scheduler)", RunE: stub("daemon")}
}
func newSecretsCmd() *cobra.Command {
	return newSecretsCmdReal()
}

func stub(name string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		cmd.Printf("mcp %s: not implemented yet\n", name)
		return nil
	}
}
