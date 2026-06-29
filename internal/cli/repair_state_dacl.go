package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

var repairStateDACLScanFn = api.FindStateFileDACLRepairCandidates
var repairStateDACLRepairFn = api.RepairStateFileDACL

func newRepairStateDACLCmd() *cobra.Command {
	var pathFlag string
	var all bool
	var yes bool
	c := &cobra.Command{
		Use:   "repair-state-dacl [--path <file>] [--all] [--yes]",
		Short: "Repair owner-only permissions on stale state files",
		Long: `Repair stale hub state files whose own file DACL or mode is broader than
the owner-only allowlist. This is an operator-initiated remediation only; it
does not trust or read file contents before repair.

With no flags, the command scans the resolved state directory, lists repair
candidates, then asks for confirmation. Use --yes in non-interactive shells.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairStateDACL(cmd, repairStateDACLOpts{
				path: pathFlag,
				all:  all,
				yes:  yes,
			})
		},
	}
	c.Flags().StringVar(&pathFlag, "path", "", "repair one state file under the resolved state directory")
	c.Flags().BoolVar(&all, "all", false, "scan the resolved state directory and repair every unsafe state file")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt (required in non-interactive shells)")
	return c
}

type repairStateDACLOpts struct {
	path string
	all  bool
	yes  bool
}

func runRepairStateDACL(cmd *cobra.Command, opts repairStateDACLOpts) error {
	if opts.path != "" && opts.all {
		return fmt.Errorf("--path and --all are mutually exclusive")
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return err
	}
	stateDirAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolve state dir %s: %w", stateDir, err)
	}

	var targets []api.StateFileDACLRepairCandidate
	if opts.path != "" {
		target, err := resolveRepairStateDACLPath(stateDirAbs, opts.path)
		if err != nil {
			return err
		}
		targets = []api.StateFileDACLRepairCandidate{{Path: target, Reason: "operator-requested path"}}
	} else {
		targets, err = repairStateDACLScanFn(stateDirAbs)
		if err != nil {
			return err
		}
	}

	if len(targets) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No state files need DACL repair.")
		return nil
	}

	printRepairStateDACLCandidates(cmd.OutOrStdout(), targets)
	if !opts.yes {
		if !inputIsTerminal(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.ErrOrStderr(), "non-interactive shell - pass --yes to confirm state-file DACL repair")
			return &forceExitError{code: 6}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\nRepair %d state file(s)? [y/N]: ", len(targets))
		if !readYesNo(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted; nothing repaired.")
			return nil
		}
	}

	var repaired, refused, unchanged int
	for _, target := range targets {
		report, err := repairStateDACLRepairFn(target.Path)
		switch report.Status {
		case api.StateFileDACLRepairStatusRepaired:
			repaired++
			fmt.Fprintf(cmd.OutOrStdout(), "repaired: %s\n", report.Path)
		case api.StateFileDACLRepairStatusUnchanged:
			unchanged++
			fmt.Fprintf(cmd.OutOrStdout(), "unchanged: %s\n", report.Path)
		case api.StateFileDACLRepairStatusRefused:
			refused++
			reason := report.Reason
			if err != nil {
				reason = err.Error()
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "refused: %s: %s\n", target.Path, reason)
		default:
			if err != nil {
				refused++
				fmt.Fprintf(cmd.ErrOrStderr(), "refused: %s: %v\n", target.Path, err)
				continue
			}
			unchanged++
			fmt.Fprintf(cmd.OutOrStdout(), "unchanged: %s\n", target.Path)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Repaired %d file(s); refused %d; unchanged %d.\n", repaired, refused, unchanged)
	if refused > 0 {
		return fmt.Errorf("repair-state-dacl refused %d file(s)", refused)
	}
	return nil
}

func resolveRepairStateDACLPath(stateDirAbs, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", fmt.Errorf("--path must not be empty")
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve --path %s: %w", requested, err)
	}
	cleanStateDir := filepath.Clean(stateDirAbs)
	cleanTarget := filepath.Clean(abs)
	if !pathIsWithinDir(cleanTarget, cleanStateDir) {
		return "", fmt.Errorf("--path %s is outside state dir %s", cleanTarget, cleanStateDir)
	}
	return cleanTarget, nil
}

func pathIsWithinDir(path, dir string) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		dir = strings.ToLower(dir)
	}
	if path == dir {
		return false
	}
	prefix := dir
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}

func printRepairStateDACLCandidates(w interface{ Write([]byte) (int, error) }, candidates []api.StateFileDACLRepairCandidate) {
	fmt.Fprintf(w, "%-8s %s\n", "ACTION", "PATH")
	for _, candidate := range candidates {
		fmt.Fprintf(w, "%-8s %s\n", "repair", candidate.Path)
		if candidate.Reason != "" {
			fmt.Fprintf(w, "         reason: %s\n", candidate.Reason)
		}
		if len(candidate.RemovedSIDs) > 0 {
			fmt.Fprintf(w, "         removed-SIDs: %s\n", strings.Join(candidate.RemovedSIDs, ", "))
		}
	}
}

func setRepairStateDACLScanForTest(fn func(string) ([]api.StateFileDACLRepairCandidate, error)) func() {
	orig := repairStateDACLScanFn
	repairStateDACLScanFn = fn
	return func() { repairStateDACLScanFn = orig }
}

func setRepairStateDACLRepairForTest(fn func(string) (api.StateFileDACLRepairReport, error)) func() {
	orig := repairStateDACLRepairFn
	repairStateDACLRepairFn = fn
	return func() { repairStateDACLRepairFn = orig }
}
