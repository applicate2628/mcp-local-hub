package cli

import (
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

type deAdoptCLIAPI interface {
	BuildDeAdoptPlan(string) (*api.DeAdoptPlan, error)
	ExecuteDeAdoptWithOpts(string, io.Writer, api.ExecuteDeAdoptOpts) (*api.DeAdoptReport, error)
}

var newDeAdoptCLIAPI = func() deAdoptCLIAPI { return api.NewAPI() }

func newDeAdoptCmdReal() *cobra.Command {
	var yes bool
	var acceptConflictClients []string
	cmd := &cobra.Command{
		Use:     "de-adopt <server>",
		Aliases: []string{"deadopt"},
		Short:   "Restore client entries and remove an adopt-owned manifest",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := newDeAdoptCLIAPI()
			plan, err := a.BuildDeAdoptPlan(args[0])
			if err != nil {
				return err
			}
			if plan == nil {
				return fmt.Errorf("de-adopt plan for %q was empty", args[0])
			}
			if plan.Routing == api.DeAdoptRoutingRefuse {
				api.PrintDeAdoptPlan(cmd.OutOrStdout(), plan)
				return fmt.Errorf("de-adopt refused: %s", plan.RefusalReason)
			}
			if !yes {
				api.PrintDeAdoptPlan(cmd.OutOrStdout(), plan)
				failures := make([]string, 0)
				for _, client := range plan.Clients {
					if client.Disposition == api.DeAdoptClientFailed {
						failures = append(failures, fmt.Sprintf("%s (%s)", client.Client, client.Reason))
					}
				}
				if len(failures) != 0 {
					return fmt.Errorf("de-adopt plan is not executable: %d client(s) would fail; %s", len(failures), strings.Join(failures, "; "))
				}
				return nil
			}

			report, err := a.ExecuteDeAdoptWithOpts(args[0], cmd.OutOrStdout(), api.ExecuteDeAdoptOpts{
				AcceptConflictClients: acceptConflictClients,
			})
			if err != nil {
				return err
			}
			if report == nil {
				return fmt.Errorf("de-adopt did not complete: executor returned no report")
			}
			if len(report.Failed) != 0 {
				failures := make([]string, 0, len(report.Failed))
				for _, failure := range report.Failed {
					failures = append(failures, fmt.Sprintf("%s (%s)", failure.Client, failure.Reason))
				}
				return fmt.Errorf("de-adopt did not complete: %d client(s) failed; %s", len(report.Failed), strings.Join(failures, "; "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "execute the de-adopt plan; without this the command is a dry-run")
	cmd.Flags().StringArrayVar(&acceptConflictClients, "accept-conflict", nil, "accept a genuine conflict for this client (repeatable)")
	return cmd
}
