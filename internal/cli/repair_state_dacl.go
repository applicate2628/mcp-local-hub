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

var repairStateDACLRepairFn = api.RepairStateFileDACL

const repairStateDACLPosixOpenWriterNotice = "POSIX note: chmod cannot revoke already-open writer file descriptors; the operator must ensure no other process already holds the file open for writing before running repair-state-dacl."

func newRepairStateDACLCmd() *cobra.Command {
	var pathFlag string
	var yes bool
	c := &cobra.Command{
		Use:   "repair-state-dacl --path <state-file> [--yes]",
		Short: "Repair owner-only permissions on stale state files",
		// Hidden: single-file DACL remediation. It is only ever needed in
		// response to a state-file permission refusal, and that refusal
		// message (api.StateFileDACLRunbookPointer) hands the operator the
		// exact `mcphub repair-state-dacl --path <file>` invocation.
		Hidden: true,
		Long: `Repair stale hub state files whose own file DACL or mode is broader than
the owner-only allowlist. This is an operator-initiated remediation only; it
does not trust or read file contents before repair.

The command repairs exactly one operator-named state file. Relative paths and
basenames are resolved under the resolved state directory. Use --yes in
non-interactive shells.

On Windows, repair first opens the target with a FILE_READ_DATA-free strong mask
that enforces a writer-exclusion guarantee. If that open is denied, repair
refuses and points to the manual icacls runbook; it still does not read file
contents.
On POSIX, chmod cannot revoke already-open writer file descriptors; the operator
must ensure no other process already holds the file open for writing before
running this command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairStateDACL(cmd, repairStateDACLOpts{
				path: pathFlag,
				yes:  yes,
			})
		},
	}
	c.Flags().StringVar(&pathFlag, "path", "", "state file to repair under the resolved state directory; relative paths are resolved there")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt (required in non-interactive shells)")
	return c
}

type repairStateDACLOpts struct {
	path string
	yes  bool
}

func runRepairStateDACL(cmd *cobra.Command, opts repairStateDACLOpts) error {
	if strings.TrimSpace(opts.path) == "" {
		return fmt.Errorf("--path is required")
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return err
	}
	stateDirAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolve state dir %s: %w", stateDir, err)
	}

	target, err := resolveRepairStateDACLPath(stateDirAbs, opts.path)
	if err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		fmt.Fprintln(cmd.OutOrStdout(), repairStateDACLPosixOpenWriterNotice)
	}
	if !opts.yes {
		if !inputIsTerminal(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.ErrOrStderr(), "non-interactive shell - pass --yes to confirm state-file DACL repair")
			return &forceExitError{code: 6}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Repair state file %s? [y/N]: ", target)
		if !readYesNo(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted; nothing repaired.")
			return nil
		}
	}

	report, err := repairStateDACLRepairFn(target)
	switch report.Status {
	case api.StateFileDACLRepairStatusRepaired:
		fmt.Fprintf(cmd.OutOrStdout(), "repaired: %s\n", report.Path)
	case api.StateFileDACLRepairStatusUnchanged:
		fmt.Fprintf(cmd.OutOrStdout(), "unchanged: %s\n", report.Path)
	case api.StateFileDACLRepairStatusRefused:
		reason := report.Reason
		if err != nil {
			reason = err.Error()
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "refused: %s: %s\n", target, reason)
		if err != nil {
			return err
		}
		return fmt.Errorf("repair-state-dacl refused %s", target)
	default:
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "refused: %s: %v\n", target, err)
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "unchanged: %s\n", target)
	}
	return nil
}

func resolveRepairStateDACLPath(stateDirAbs, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", fmt.Errorf("--path must not be empty")
	}
	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(stateDirAbs, target)
	}
	abs, err := filepath.Abs(target)
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

func setRepairStateDACLRepairForTest(fn func(string) (api.StateFileDACLRepairReport, error)) func() {
	orig := repairStateDACLRepairFn
	repairStateDACLRepairFn = fn
	return func() { repairStateDACLRepairFn = orig }
}
