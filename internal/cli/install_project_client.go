package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"mcp-local-hub/internal/api"
)

func parseInstallProjectClientControls(cmd *cobra.Command, server, daemon, clientFlag string, dry, all, allClients, reconcileHub, reconcileFront, rollback, upgrade, check, noClient bool, root, entry, policy string, create bool, digest string) (*api.InstallProjectClientCreate, error) {
	names := []string{"project-client-root", "project-client-entry", "project-client-policy-source", "create-project-client", "project-client-plan-sha256"}
	anyControl := false
	for _, name := range names {
		anyControl = anyControl || cmd.Flags().Changed(name)
	}
	if !anyControl {
		return nil, nil
	}
	if !cmd.Flags().Changed("server") || !cmd.Flags().Changed("daemon") || !cmd.Flags().Changed("clients") || clientFlag != "codex-cli" || all || allClients || reconcileHub || reconcileFront || rollback || upgrade || check || noClient || (cmd.Flags().Changed("create-project-client") && !create) {
		return nil, fmt.Errorf("PROJECT_CLIENT_INVALID: prospective attachment controls require one explicit server, daemon and codex-cli client")
	}
	c := &api.InstallProjectClientCreate{ProjectRoot: root, LogicalEntryName: entry, PolicySource: policy, ApplyCreate: create, ExpectedPlanSHA256: digest}
	if err := api.ValidateInstallProjectClientCreate(api.InstallOpts{Server: server, DaemonFilter: daemon, ClientsInclude: []string{"codex-cli"}, DryRun: dry, ProjectClientCreate: c}); err != nil {
		return nil, err
	}
	return c, nil
}

func parseInstallProjectClientRollbackControls(cmd *cobra.Command, dry, all, allClients, reconcileHub, reconcileFront, rollback, upgrade, check, noClient bool, root, entry, generation string, apply bool) (*api.InstallProjectClientRollback, error) {
	if all || allClients || reconcileHub || reconcileFront || rollback || upgrade || check || noClient {
		return nil, fmt.Errorf("PROJECT_CLIENT_INVALID: rollback controls are exclusive")
	}
	for _, name := range []string{"server", "daemon", "clients", "project-client-policy-source", "create-project-client", "project-client-plan-sha256"} {
		if cmd.Flags().Changed(name) {
			return nil, fmt.Errorf("PROJECT_CLIENT_INVALID: rollback conflicts with %s", name)
		}
	}
	if cmd.Flags().Changed("apply-project-client-rollback") && !apply {
		return nil, fmt.Errorf("PROJECT_CLIENT_INVALID: explicit false rollback authority")
	}
	c := &api.InstallProjectClientRollback{ProjectRoot: root, LogicalEntryName: entry, GenerationID: generation, Apply: apply}
	if err := api.ValidateInstallProjectClientRollback(api.InstallOpts{ProjectClientRollback: c, DryRun: dry}); err != nil {
		return nil, err
	}
	return c, nil
}
