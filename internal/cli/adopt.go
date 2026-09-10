package cli

import (
	"fmt"
	"os"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newAdoptCmdReal() *cobra.Command {
	return newAdoptCmdWithDeps(api.NewAPI, nil, nil)
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
	var nameFlag string
	var clientsFlag string
	var compatibilityProfileFlag string
	var portFlag int
	var yes bool
	cmd := &cobra.Command{
		Use:   "adopt <entry-name>",
		Short: "Absorb a direct stdio MCP client entry into the hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer installInteractiveSymlinkConsent(cmd.OutOrStdout(), os.Stdin)()

			if strings.TrimSpace(clientFlag) == "" {
				return fmt.Errorf("--client is required (%s)", strings.Join(api.AdoptSupportedClients(), " | "))
			}
			include, err := parseInstallClientsFlag(clientsFlag, false)
			if err != nil {
				return err
			}
			a := newAPI()
			opts := api.AdoptOpts{
				EntryName:                               args[0],
				Client:                                  clientFlag,
				ManifestName:                            nameFlag,
				Port:                                    portFlag,
				Clients:                                 include,
				MCPProtocolCompatibilityProfile:         compatibilityProfileFlag,
				MCPProtocolCompatibilityProfileExplicit: cmd.Flags().Changed("mcp-protocol-compatibility-profile"),
				ProviderPluginRef:                       providerPluginFlag,
			}
			builder := buildPlan
			if builder == nil {
				builder = (*api.API).BuildAdoptPlan
			}
			plan, err := builder(a, opts)
			if err != nil {
				return err
			}
			if !yes {
				namespace, err := a.PreflightAdoptPlan(plan)
				if err != nil {
					return err
				}
				api.PrintAdoptPlan(cmd.OutOrStdout(), plan)
				if namespace.MigrationEligible {
					fmt.Fprintf(cmd.OutOrStdout(), "  lease namespace migration: required at apply (state=%s reason_id=%s action=%s)\n", namespace.State, namespace.ReasonID, namespace.Action)
				}
				return nil
			}
			return a.ExecuteAdoptWithOpts(plan, cmd.OutOrStdout(), api.ExecuteAdoptOpts{LeaseOwner: leaseOwner, ReceivingVerifier: receivingVerifier})
		},
	}
	cmd.Flags().StringVar(&clientFlag, "client", "", "source client ("+strings.Join(api.AdoptSupportedClients(), " | ")+")")
	cmd.Flags().StringVar(&providerPluginFlag, "provider-plugin", "", "explicit provider-owned plugin reference")
	cmd.Flags().StringVar(&nameFlag, "name", "", "manifest name (default: entry name; v1 requires it to match)")
	cmd.Flags().IntVar(&portFlag, "port", 0, "hub daemon port (default: first free 9300-9399)")
	cmd.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated clients to repoint (default: every same-name direct entry found)")
	cmd.Flags().StringVar(&compatibilityProfileFlag, "mcp-protocol-compatibility-profile", "", "optional stdio MCP protocol compatibility profile")
	cmd.Flags().BoolVar(&yes, "yes", false, "execute the adopt plan; without this the command is a dry-run")
	return cmd
}
