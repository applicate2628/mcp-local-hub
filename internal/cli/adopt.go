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
	var clientFlag string
	var nameFlag string
	var clientsFlag string
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
			plan, err := a.BuildAdoptPlan(api.AdoptOpts{
				EntryName:    args[0],
				Client:       clientFlag,
				ManifestName: nameFlag,
				Port:         portFlag,
				Clients:      include,
			})
			if err != nil {
				return err
			}
			if !yes {
				api.PrintAdoptPlan(cmd.OutOrStdout(), plan)
				return nil
			}
			return a.ExecuteAdoptWithOpts(plan, cmd.OutOrStdout(), api.ExecuteAdoptOpts{LeaseOwner: leaseOwner, ReceivingVerifier: receivingVerifier})
		},
	}
	cmd.Flags().StringVar(&clientFlag, "client", "", "source client ("+strings.Join(api.AdoptSupportedClients(), " | ")+")")
	cmd.Flags().StringVar(&nameFlag, "name", "", "manifest name (default: entry name; v1 requires it to match)")
	cmd.Flags().IntVar(&portFlag, "port", 0, "hub daemon port (default: first free 9300-9399)")
	cmd.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated clients to repoint (default: every same-name direct entry found)")
	cmd.Flags().BoolVar(&yes, "yes", false, "execute the adopt plan; without this the command is a dry-run")
	return cmd
}
