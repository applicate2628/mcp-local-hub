package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

type testLeftoverPreviewRunner func(api.TestLeftoverPreviewOpts) (api.TestLeftoverPreview, error)

func newCleanupTestLeftoversCmdReal() *cobra.Command {
	a := api.NewAPI()
	return newCleanupTestLeftoversCmd(a.PreviewTestLeftovers)
}

// newCleanupTestLeftoversCmd builds the explicit V1 evidence command. The
// injected runner keeps CLI tests hermetic; production supplies the read-only
// API method above.
func newCleanupTestLeftoversCmd(run testLeftoverPreviewRunner) *cobra.Command {
	var minAgeSec int64
	var tempRoot string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "test-leftovers",
		Short: "Preview test-leftover process evidence without taking action",
		Long: `Enumerate process rows resembling known mcphub test-leftover families and
print read-only identity, path, argv, age, parent-liveness, and buildinfo
evidence. This V1 command is preview-only. It has no apply or confirmation
mode, and standalone supervise rows require out-of-band identity verification.`,
		Args: cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if minAgeSec < 0 {
				return fmt.Errorf("cleanup test-leftovers: --min-age-sec must be non-negative")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := run(api.TestLeftoverPreviewOpts{
				MinAgeSec: minAgeSec,
				TempRoot:  tempRoot,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			printTestLeftoverPreviewHuman(cmd, result)
			return nil
		},
	}
	cmd.Flags().Int64Var(&minAgeSec, "min-age-sec", api.DefaultTestLeftoverMinAgeSec,
		"diagnostic age threshold in seconds (labels only; candidates remain visible)")
	cmd.Flags().StringVar(&tempRoot, "temp-root", "",
		"strict-canonical diagnostic scope for reliability-family images")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "machine-readable JSON output")
	return cmd
}

func printTestLeftoverPreviewHuman(cmd *cobra.Command, result api.TestLeftoverPreview) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "test-leftover preview: snapshot=%s  exhaustive=%t  requested-min-age=%ds  temp-root=%s  protected-scopes=%s\n",
		result.SnapshotVerdict, result.Exhaustive, result.RequestedMinAgeSec, result.TempRootVerdict,
		formatTestLeftoverProtectedScopeVerdicts(result.ProtectedScopeVerdicts))
	if !result.Exhaustive {
		diagnostic := strings.TrimSpace(stripTerminalControls(result.SnapshotDiagnostic))
		if diagnostic == "" {
			diagnostic = "snapshot degraded; candidate list is not exhaustive"
		}
		fmt.Fprintf(out, "warning: %s (not exhaustive)\n", diagnostic)
	}

	for _, candidate := range result.Candidates {
		startedAt := candidate.StartedAt
		if startedAt == "" {
			startedAt = "unavailable"
		}
		executable := candidate.ExecutablePath
		if strings.TrimSpace(executable) == "" {
			executable = candidate.ExecutableDisplay
		}
		relations := strings.Join(candidate.PathRelations, ",")
		if relations == "" {
			relations = "none"
		}
		age := fmt.Sprintf("%ds", candidate.AgeSec)
		if candidate.AgeSec < 0 {
			age = "unavailable"
		}
		fmt.Fprintf(out, "\nPID %d  parent=%d  started=%s  age=%s (%s; %s)\n",
			candidate.PID, candidate.ParentPID, stripTerminalControls(startedAt), age,
			candidate.AgeVsRequestedMin, candidate.AgeVsApplyFloor)
		fmt.Fprintf(out, "  executable: %s\n", stripTerminalControls(executable))
		fmt.Fprintf(out, "  argv=%s  image=%s  class=%s\n",
			candidate.ArgvShape, candidate.ImageFamily, candidate.PatternClass)
		fmt.Fprintf(out, "  identity=%s  parent=%s  path=%s  relations=%s\n",
			candidate.IdentityVerdict, candidate.ParentLiveness, candidate.PathVerdict, relations)
		fmt.Fprintf(out, "  buildinfo=%s  environment=%s\n",
			candidate.BuildInfoTag, candidate.EnvironmentOverride)
		fmt.Fprintf(out, "  lifecycle=%s  would-refuse=%s\n",
			candidate.ApplyLifecycle, candidate.WouldRefuse)
		if candidate.OperatorNote != "" {
			fmt.Fprintf(out, "  note: %s\n", stripTerminalControls(candidate.OperatorNote))
		}
	}

	fmt.Fprintf(out, "\n%d candidate(s) · %s · preview only; no process action is performed\n",
		len(result.Candidates), api.TestLeftoverApplyDeferred)
}

func formatTestLeftoverProtectedScopeVerdicts(verdicts map[string]string) string {
	names := make([]string, 0, len(verdicts))
	for name := range verdicts {
		names = append(names, name)
	}
	sort.Strings(names)
	formatted := make([]string, 0, len(names))
	for _, name := range names {
		formatted = append(formatted, name+":"+verdicts[name])
	}
	if len(formatted) == 0 {
		return "none"
	}
	return strings.Join(formatted, ",")
}
