package cli

import (
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/drmemory"
	"mcp-local-hub/internal/gdb"
	"mcp-local-hub/internal/godbolt"
	"mcp-local-hub/internal/lldb"
	"mcp-local-hub/internal/oneapirun"
	"mcp-local-hub/internal/perftools"
	"mcp-local-hub/internal/vtune"
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
	root.AddCommand(newUpgradeCmd())
	root.AddCommand(newSetupCmd())
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
	root.AddCommand(newBackupsCmd())
	root.AddCommand(newManifestCmd())
	root.AddCommand(newSchedulerCmd())
	root.AddCommand(newSettingsCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newRegisterCmd())
	root.AddCommand(newUnregisterCmd())
	root.AddCommand(newWorkspacesCmd())
	root.AddCommand(newWorkspaceCmd())
	root.AddCommand(newLanguageServerCmd())
	root.AddCommand(newMigrateLegacyCmd())
	root.AddCommand(newImportCmd())
	root.AddCommand(newWeeklyRefreshCmd())
	root.AddCommand(newGuiCmd())
	root.AddCommand(newTrayCmd())
	root.AddCommand(newSuperviseCmd())
	root.AddCommand(newReconcileCmd())
	root.AddCommand(newIntentCollapseCmd())
	root.AddCommand(newAutostartCmd())
	root.AddCommand(newStrictModeCmd())
	root.AddCommand(newHubMcpCmd())
	root.AddCommand(newMarketplaceCmd())
	root.AddCommand(lldb.NewCommand())
	root.AddCommand(gdb.NewCommand())
	root.AddCommand(godbolt.NewCommand())
	root.AddCommand(perftools.NewCommand())
	root.AddCommand(oneapirun.NewCommand())
	root.AddCommand(drmemory.NewCommand())
	root.AddCommand(vtune.NewCommand())
	return root
}

// Stub constructors — each returns a cobra.Command that prints "not implemented yet".
// Later tasks replace each RunE with real logic.
func newInstallCmd() *cobra.Command {
	return newInstallCmdReal()
}
func newUpgradeCmd() *cobra.Command   { return newUpgradeCmdReal() }
func newSetupCmd() *cobra.Command     { return newSetupCmdReal() }
func newUninstallCmd() *cobra.Command { return newUninstallCmdReal() }
func newStatusCmd() *cobra.Command    { return newStatusCmdReal() }
func newScanCmd() *cobra.Command      { return newScanCmdReal() }
func newMigrateCmd() *cobra.Command   { return newMigrateCmdReal() }
func newRestartCmd() *cobra.Command   { return newRestartCmdReal() }
func newRollbackCmd() *cobra.Command  { return newRollbackCmdReal() }
func newDaemonCmd() *cobra.Command    { return newDaemonCmdReal() }
func newRelayCmd() *cobra.Command     { return newRelayCmdReal() }
func newVersionCmd() *cobra.Command   { return newVersionCmdReal() }
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
func newBackupsCmd() *cobra.Command    { return newBackupsCmdReal() }
func newManifestCmd() *cobra.Command   { return newManifestCmdReal() }
func newSchedulerCmd() *cobra.Command  { return newSchedulerCmdReal() }
func newSettingsCmd() *cobra.Command   { return newSettingsCmdReal() }
func newConfigCmd() *cobra.Command     { return newConfigCmdReal() }
func newRegisterCmd() *cobra.Command   { return newRegisterCmdReal() }
func newUnregisterCmd() *cobra.Command { return newUnregisterCmdReal() }
func newWorkspacesCmd() *cobra.Command { return newWorkspacesCmdReal() }
func newLanguageServerCmd() *cobra.Command {
	return newLanguageServerCmdReal()
}
func newMigrateLegacyCmd() *cobra.Command { return newMigrateLegacyCmdReal() }
func newImportCmd() *cobra.Command        { return newImportCmdReal() }
func newWeeklyRefreshCmd() *cobra.Command { return newWeeklyRefreshCmdReal() }
func newGuiCmd() *cobra.Command           { return newGuiCmdReal() }
func newTrayCmd() *cobra.Command          { return newTrayCmdReal() }
func newReconcileCmd() *cobra.Command     { return newReconcileCmdReal() }

func newIntentCollapseCmd() *cobra.Command { return newIntentCollapseCmdReal() }
