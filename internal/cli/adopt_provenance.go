package cli

import (
	"fmt"
	"io"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

type forgetProvenanceCLIAPI interface {
	BuildForgetAdoptProvenancePlan(string) (*api.ForgetAdoptProvenancePlan, error)
	ForgetAdoptProvenance(string, api.ForgetAdoptProvenanceOpts) (*api.ForgetAdoptProvenancePlan, error)
}

type profileUpdateCLIAPI interface {
	BuildAdoptProfileUpdatePlan(string, string) (*api.AdoptProfileUpdatePlan, error)
	ExecuteAdoptProfileUpdate(*api.AdoptProfileUpdatePlan, api.AdoptProfileUpdateOpts) error
}

type realProfileUpdateCLIAPI struct{ *api.API }

var newProfileUpdateCLIAPI = func() profileUpdateCLIAPI { return realProfileUpdateCLIAPI{api.NewAPI()} }

var newForgetProvenanceCLIAPI = func() forgetProvenanceCLIAPI { return api.NewAPI() }

var inspectAdoptLeaseNamespaceCLI = api.InspectAdoptLeaseNamespace
var migrateLegacyAdoptLeaseNamespaceCLI = api.MigrateLegacyAdoptLeaseNamespace

func newAdoptProvenanceCmdReal() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt-provenance",
		Short: "Inspect and manage adopt-provenance records",
		// Hidden: record-store maintenance subordinate to `adopt`/`de-adopt`.
		// The records are captured and reclaimed automatically (UPSERT on
		// re-adopt, 24h GC); this is the manual hatch for a stale or
		// blocking record.
		Hidden: true,
		Long: "Adopt-provenance records are the durable pre-adopt snapshots `mcphub adopt` " +
			"captures so `mcphub de-adopt` can restore a client to its exact pre-adopt config. " +
			"These subcommands manage stale or blocking records.",
	}
	cmd.AddCommand(newAdoptProvenanceForgetCmd())
	cmd.AddCommand(newAdoptProfileUpdateCmd())
	cmd.AddCommand(newAdoptLeaseNamespaceCmd())
	return cmd
}

func newAdoptProfileUpdateCmd() *cobra.Command {
	var profile string
	var yes bool
	cmd := &cobra.Command{
		Use:   "update-profile <manifest>",
		Short: "Update the adopted stdio MCP protocol compatibility profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := newProfileUpdateCLIAPI()
			plan, err := a.BuildAdoptProfileUpdatePlan(args[0], profile)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adopt-provenance update-profile %s\nprofile=%s\nold_manifest_hash=%s\nnew_manifest_hash=%s\nrestart_required=%t\n", plan.ManifestName, plan.Profile, plan.OldManifestHash, plan.NewManifestHash, plan.RestartRequired)
			if !yes {
				fmt.Fprintln(cmd.OutOrStdout(), "dry_run=true mutation=false")
				return nil
			}
			if err := a.ExecuteAdoptProfileUpdate(plan, api.AdoptProfileUpdateOpts{}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "updated=true restart_required=true restart_performed=false")
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "mcp-protocol-compatibility-profile", "", "required stdio MCP protocol compatibility profile")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply the reviewed update; without this flag the command is a dry-run")
	_ = cmd.MarkFlagRequired("mcp-protocol-compatibility-profile")
	return cmd
}

func newAdoptLeaseNamespaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "lease-namespace",
		Short:  "Inspect or migrate the Windows adopt lease namespace",
		Hidden: true,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "inspect",
		Short: "Read the lease namespace classification without changing it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := inspectAdoptLeaseNamespaceCLI()
			printAdoptLeaseNamespaceReport(cmd.OutOrStdout(), report)
			return err
		},
	})
	cmd.AddCommand(newAdoptLeaseNamespaceMigrateLegacyCmd())
	return cmd
}

func newAdoptLeaseNamespaceMigrateLegacyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "migrate-legacy",
		Short: "Tighten a positively verified legacy lease namespace",
		Long: "Validates the entire namespace before changing any DACL. Only current-user-owned, " +
			"real legacy lease leaves and already-safe snapshot siblings are accepted. " +
			"The command never deletes, renames, or changes file content.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := migrateLegacyAdoptLeaseNamespaceCLI(api.AdoptLeaseNamespaceMigrationOpts{Yes: yes})
			printAdoptLeaseNamespaceReport(cmd.OutOrStdout(), report)
			if err != nil {
				return err
			}
			if !yes && report.MigrationEligible {
				fmt.Fprintln(cmd.OutOrStdout(), "dry_run=true mutation=false")
				fmt.Fprintln(cmd.OutOrStdout(), "apply=mcphub adopt-provenance lease-namespace migrate-legacy --yes")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply the validated migration; without this flag the command is read-only")
	return cmd
}

func printAdoptLeaseNamespaceReport(w io.Writer, report api.AdoptLeaseNamespaceReport) {
	fmt.Fprintf(w, "state=%s reason_id=%s action=%s migration_eligible=%t lease_leaves=%d snapshot_dirs=%d\n",
		report.State, report.ReasonID, report.Action, report.MigrationEligible, report.LeaseLeafCount, report.SnapshotDirCount)
	if report.ChangedLeafCount > 0 || report.NamespaceChanged || report.RollbackPerformed {
		fmt.Fprintf(w, "changed_leaves=%d namespace_changed=%t rollback_performed=%t\n",
			report.ChangedLeafCount, report.NamespaceChanged, report.RollbackPerformed)
	}
}

func newAdoptProvenanceForgetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget <manifest>",
		Short: "Discard a stale/blocking adopt-provenance row + its snapshot dir",
		Long: "Removes the durable provenance row and pinned snapshot dir for <manifest> under " +
			"the per-manifest lease. Use this to clear a provenance record the reap predicate " +
			"conservatively keeps (for example a crashed adopt) when you do not need to de-adopt it.\n\n" +
			"This is DESTRUCTIVE and discards provenance bookkeeping only: it does NOT restore any " +
			"client config and does NOT remove routed vault keys (those are de-adopt's job).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest := args[0]
			a := newForgetProvenanceCLIAPI()
			plan, err := a.BuildForgetAdoptProvenancePlan(manifest)
			if err != nil {
				return err
			}
			printForgetPlan(cmd.OutOrStdout(), plan)
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nDry run — nothing removed. Re-run with --yes to forget:\n  mcphub adopt-provenance forget %s --yes\n",
					manifest)
				return nil
			}
			done, err := a.ForgetAdoptProvenance(manifest, api.ForgetAdoptProvenanceOpts{
				Yes: true,
				// Gate the removal on exactly the row (or exact absence of a row) the operator
				// just reviewed: if it changed — or a row appeared where 'row: none' was shown —
				// between this dry-run read and the act, the API errors instead of destroying
				// something never displayed.
				ConfirmIdentity:   true,
				ExpectedHasRow:    plan.HasRow,
				ExpectedRowState:  plan.RowState,
				ExpectedUpdatedAt: plan.UpdatedAt,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nForgotten: removed the provenance record for %q", done.ManifestName)
			if done.HasSnapshotDir {
				fmt.Fprint(cmd.OutOrStdout(), " + its snapshot dir")
			}
			fmt.Fprintln(cmd.OutOrStdout(), ".")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation dry-run and remove immediately")
	return cmd
}

func printForgetPlan(w io.Writer, plan *api.ForgetAdoptProvenancePlan) {
	fmt.Fprintf(w, "adopt-provenance forget %s\n", plan.ManifestName)
	if plan.HasRow {
		fmt.Fprintf(w, "  row:      present (state=%s)\n", plan.RowState)
		if len(plan.Clients) > 0 {
			fmt.Fprintf(w, "  clients:  %s\n", strings.Join(plan.Clients, ", "))
		}
	} else {
		fmt.Fprintln(w, "  row:      none")
	}
	if plan.HasSnapshotDir {
		fmt.Fprintf(w, "  snapshot: %s\n", plan.SnapshotDir)
	} else {
		fmt.Fprintln(w, "  snapshot: none")
	}
	if len(plan.RoutedSecretKeys) > 0 {
		// forget does NOT delete these — surface the names so the operator can clean the
		// vault manually (via `mcphub secrets`) once the row is gone.
		fmt.Fprintf(w, "  vault:    %s (NOT removed by forget — clean manually via `mcphub secrets`)\n",
			strings.Join(plan.RoutedSecretKeys, ", "))
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "  WARNING:  %s\n", warn)
	}
}
