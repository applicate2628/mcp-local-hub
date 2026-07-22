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
// Command group IDs for the `mcphub --help` listing. Grouping is a
// ROOT-level layout concern and is therefore owned here; per-command
// VISIBILITY (`Hidden`) stays with each command's own constructor, matching
// the existing idiom in tray_cmd.go / weekly_refresh.go.
//
// Every VISIBLE command must be assigned a group: cobra collects ungrouped
// available commands under a generic "Additional Commands" heading, which
// would reintroduce exactly the undifferentiated list this grouping removes.
const (
	groupSetup       = "setup"
	groupServers     = "servers"
	groupRuntime     = "runtime"
	groupSecrets     = "secrets"
	groupMaintenance = "maintenance"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mcphub",
		Short: "Local shared-daemon manager for MCP servers",
		Long: `mcphub runs your MCP servers as shared local daemons instead of one
process per client, and points every MCP client at them.

Run "mcphub" with no arguments to start the hub and open the GUI.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Groups must be registered BEFORE any command claiming them: cobra's
	// AddCommand panics on a GroupID with no matching group.
	root.AddGroup(
		&cobra.Group{ID: groupSetup, Title: "Setup:"},
		&cobra.Group{ID: groupServers, Title: "MCP servers:"},
		&cobra.Group{ID: groupRuntime, Title: "Running the hub:"},
		&cobra.Group{ID: groupSecrets, Title: "Secrets:"},
		&cobra.Group{ID: groupMaintenance, Title: "Maintenance:"},
	)

	addGrouped(root, groupSetup,
		newSetupCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newUpgradeCmd(),
		newAutostartCmd(),
		newImportCmd(),
	)
	addGrouped(root, groupServers,
		newScanCmd(),
		newAdoptCmd(),
		newDeAdoptCmd(),
		newMigrateCmd(),
		newManifestCmd(),
		newMarketplaceCmd(),
		newLanguageServerCmd(),
		newLSPRouterCmd(),
		newRegisterCmd(),
		newUnregisterCmd(),
		newWorkspaceCmd(),
		newWorkspacesCmd(),
		newTrustCmd(),
		newUntrustCmd(),
	)
	addGrouped(root, groupRuntime,
		newGuiCmd(),
		newStatusCmd(),
		newRestartCmd(),
		newStopCmd(),
		newLogsCmd(),
		newSuperviseCmd(),
	)
	addGrouped(root, groupSecrets,
		newSecretsCmd(),
	)
	addGrouped(root, groupMaintenance,
		newBackupsCmd(),
		newRollbackCmd(),
		newCleanupCmd(),
		newConfigCmd(),
		newSettingsCmd(),
		newVersionCmd(),
		// Advanced repair tools. Operator-facing but rarely needed, and —
		// unlike `daemon recover`, `repair-state-dacl`, `strict-mode
		// --recover` and `hub-mcp regenerate-*` — NOT quoted in any runtime
		// error, so the listing and shell completion are their only
		// discovery surfaces. Each constructor carries the visibility
		// rationale next to its own decision.
		newSchedulerCmd(),
		newReconcileCmd(),
	)

	// Internal / machine-invoked / advanced-diagnostic surfaces. Each sets
	// Hidden in its own constructor, so these still work exactly as before
	// (including `<cmd> --help`); they are only omitted from the top-level
	// listing. They need no GroupID — cobra never renders an unavailable
	// command, so they cannot fall into "Additional Commands".
	//
	// Hiding costs MORE than the help listing: cobra's IsAvailableCommand()
	// is false for a hidden command, so hidden commands are absent from
	// shell tab-completion too (cobra v1.10.2 completions.go:518). Each
	// entry below is either machine-invoked, or named verbatim in the
	// runtime error that calls for it, or carries an explicit
	// accepted-loss note in its own constructor.
	root.AddCommand(
		newDaemonCmd(),
		newRelayCmd(),
		newWeeklyRefreshCmd(),
		newIntentCollapseCmd(),
		newStrictModeCmd(),
		newRepairStateDACLCmd(),
		newHubMcpCmd(),
		newAdoptProvenanceCmd(),
		newMigrateLegacyCmd(),
		newTrayCmd(),
		// Binary-only canonicalize entry point. Hidden: the documented
		// operator-facing equivalent is `mcphub setup`.
		newCanonicalizeCmdReal(),
	)

	// Debugger / profiler MCP bridges. Each already sets Hidden in its own
	// package; they are machine-invoked bridge entry points, not operator
	// commands.
	root.AddCommand(
		lldb.NewCommand(),
		gdb.NewCommand(),
		godbolt.NewCommand(),
		perftools.NewCommand(),
		oneapirun.NewCommand(),
		drmemory.NewCommand(),
		vtune.NewCommand(),
	)

	// cobra's auto-generated `help` and `completion` are available commands
	// too; without an explicit group they render under "Additional
	// Commands". Park them in Maintenance so the listing has no catch-all.
	root.SetHelpCommandGroupID(groupMaintenance)
	root.SetCompletionCommandGroupID(groupMaintenance)

	return root
}

// addGrouped stamps groupID on each command and registers it with root.
// Assigning the group at registration keeps the whole help layout readable
// in one place instead of scattering GroupID across ~40 constructor files.
func addGrouped(root *cobra.Command, groupID string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.GroupID = groupID
		root.AddCommand(c)
	}
}

// Stub constructors — each returns a cobra.Command that prints "not implemented yet".
// Later tasks replace each RunE with real logic.
func newInstallCmd() *cobra.Command {
	return newInstallCmdReal()
}
func newAdoptCmd() *cobra.Command     { return newAdoptCmdReal() }
func newDeAdoptCmd() *cobra.Command   { return newDeAdoptCmdReal() }

func newAdoptProvenanceCmd() *cobra.Command { return newAdoptProvenanceCmdReal() }
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
func newLSPRouterCmd() *cobra.Command     { return newLSPRouterCmdReal() }
func newMigrateLegacyCmd() *cobra.Command { return newMigrateLegacyCmdReal() }
func newImportCmd() *cobra.Command        { return newImportCmdReal() }
func newWeeklyRefreshCmd() *cobra.Command { return newWeeklyRefreshCmdReal() }
func newGuiCmd() *cobra.Command           { return newGuiCmdReal() }
func newTrayCmd() *cobra.Command          { return newTrayCmdReal() }
func newReconcileCmd() *cobra.Command     { return newReconcileCmdReal() }

func newIntentCollapseCmd() *cobra.Command { return newIntentCollapseCmdReal() }
