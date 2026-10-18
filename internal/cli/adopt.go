package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newAdoptCmdReal() *cobra.Command {
	return newAdoptCmdWithDeps(func() *api.API {
		return api.NewAPIWithSerenaAndProjectServicePreparation(nil, newProjectServiceLaunchPreparationReader())
	}, nil, nil)
}

// newAdoptCmdWithDeps keeps the CLI as a thin composition layer: the default
// real command supplies no owner and ExecuteAdoptWithOpts binds the production
// lease owner. Alternate in-process compositions can supply an owner while
// still exercising the same Cobra command and API transaction.
func newAdoptCmdWithDeps(newAPI func() *api.API, leaseOwner api.AdoptLeaseOwner, receivingVerifier api.AdoptReceivingVerifier) *cobra.Command {
	return newAdoptCmdWithDepsAndPlanBuilder(newAPI, leaseOwner, receivingVerifier, nil)
}

func newAdoptCmdWithDepsAndPlanBuilder(newAPI func() *api.API, leaseOwner api.AdoptLeaseOwner, receivingVerifier api.AdoptReceivingVerifier, buildPlan func(*api.API, api.AdoptOpts) (*api.AdoptPlan, error)) *cobra.Command {
	var clientFlag string
	var providerPluginFlag string
	var projectSourceFlag string
	var projectServiceFlag string
	var projectServiceIntentFlag string
	var nameFlag string
	var clientsFlag string
	var compatibilityProfileFlag string
	var portFlag int
	var actionTimeout string
	var yes bool
	cmd := &cobra.Command{
		Use:   "adopt <entry-name>",
		Short: "Absorb a direct stdio MCP client entry into the hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			defer installInteractiveSymlinkConsent(cmd.OutOrStdout(), os.Stdin)()

			if strings.TrimSpace(clientFlag) == "" {
				return fmt.Errorf("--client is required (%s)", strings.Join(api.AdoptSupportedClients(), " | "))
			}
			if projectSourceFlag != "" {
				if clientFlag != "codex-cli" {
					return fmt.Errorf("--project-source requires --client codex-cli")
				}
				if cmd.Flags().Changed("provider-plugin") {
					return fmt.Errorf("--project-source cannot be combined with --provider-plugin")
				}
			}
			common := cmd.Flags().Changed("project-service") || cmd.Flags().Changed("project-service-intent")
			if common && (cmd.Flags().Changed("name") || cmd.Flags().Changed("clients") || cmd.Flags().Changed("provider-plugin")) {
				return fmt.Errorf("project-service mode does not accept legacy name/client-fanout/provider selectors")
			}
			include, err := parseInstallClientsFlag(clientsFlag, false)
			if err != nil {
				return err
			}
			actionCtx := cmd.Context()
			if cmd.Flags().Changed("action-timeout") {
				var cancel context.CancelFunc
				actionCtx, cancel, err = api.ResolveCodexSharedStdioActionContext(actionCtx, &actionTimeout)
				if err != nil {
					return err
				}
				defer cancel()
			}
			a := newAPI()
			defer func() {
				if closeErr := a.CloseStdioBridgeAdmissions(context.Background()); closeErr != nil {
					err = errors.Join(err, closeErr)
				}
			}()
			opts := api.AdoptOpts{
				Context:                                 actionCtx,
				EntryName:                               args[0],
				Client:                                  clientFlag,
				ManifestName:                            nameFlag,
				Port:                                    portFlag,
				Clients:                                 include,
				MCPProtocolCompatibilityProfile:         compatibilityProfileFlag,
				MCPProtocolCompatibilityProfileExplicit: cmd.Flags().Changed("mcp-protocol-compatibility-profile"),
				ProviderPluginRef:                       providerPluginFlag,
				CodexProjectSourceRoot:                  projectSourceFlag,
			}
			var plan *api.AdoptPlan
			if common {
				plan, err = a.BuildProjectServicePlan(actionCtx, api.ProjectServiceOpts{
					Service: api.ProjectServiceKind(projectServiceFlag), Intent: api.ProjectServiceIntent(projectServiceIntentFlag),
					EntryName: args[0], Client: clientFlag, ProjectSourceRoot: projectSourceFlag, Port: portFlag,
					MCPProtocolCompatibilityProfile:         compatibilityProfileFlag,
					MCPProtocolCompatibilityProfileExplicit: cmd.Flags().Changed("mcp-protocol-compatibility-profile"),
				})
			} else {
				builder := buildPlan
				if builder == nil {
					builder = (*api.API).BuildAdoptPlan
				}
				plan, err = builder(a, opts)
			}
			if err != nil {
				return err
			}
			if !yes {
				namespace, err := a.PreflightAdoptPlan(plan)
				if err != nil {
					return err
				}
				if plan.ProjectService != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  project service: %s / %s / %s\n", plan.ProjectService.Service, plan.ProjectService.Intent, plan.ProjectService.State)
				}
				api.PrintAdoptPlan(cmd.OutOrStdout(), plan)
				if namespace.MigrationEligible {
					fmt.Fprintf(cmd.OutOrStdout(), "  lease namespace migration: required at apply (state=%s reason_id=%s action=%s)\n", namespace.State, namespace.ReasonID, namespace.Action)
				}
				return nil
			}
			return a.ExecuteAdoptWithOpts(plan, cmd.OutOrStdout(), api.ExecuteAdoptOpts{Context: actionCtx, LeaseOwner: leaseOwner, ReceivingVerifier: receivingVerifier})
		},
	}
	cmd.Flags().StringVar(&clientFlag, "client", "", "source client ("+strings.Join(api.AdoptSupportedClients(), " | ")+")")
	cmd.Flags().StringVar(&providerPluginFlag, "provider-plugin", "", "explicit provider-owned plugin reference")
	cmd.Flags().StringVar(&projectServiceFlag, "project-service", "", "explicit project service: graphify or graft")
	cmd.Flags().StringVar(&projectServiceIntentFlag, "project-service-intent", "", "explicit project operation: define-stopped, attach-target, detach-target or retire")
	cmd.Flags().StringVar(&projectSourceFlag, "project-source", "", "absolute project root containing the Codex source entry")
	cmd.Flags().StringVar(&nameFlag, "name", "", "manifest name (default: entry name; v1 requires it to match)")
	cmd.Flags().IntVar(&portFlag, "port", 0, "hub daemon port (default: first free 9300-9399)")
	cmd.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated clients to repoint (default: every same-name direct entry found)")
	cmd.Flags().StringVar(&compatibilityProfileFlag, "mcp-protocol-compatibility-profile", "", "optional stdio MCP protocol compatibility profile")
	cmd.Flags().StringVar(&actionTimeout, "action-timeout", "", "positive Go duration for a shared stdio action; no default")
	cmd.Flags().BoolVar(&yes, "yes", false, "execute the adopt plan; without this the command is a dry-run")
	return cmd
}
