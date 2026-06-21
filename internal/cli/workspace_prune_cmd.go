// internal/cli/workspace_prune_cmd.go
//
// `mcphub workspace prune [--dry-run] [--idle <dur>] [--backend all] [--yes]
// [--json]` — a manual, bulk counterpart to the in-GUI auto-prune sweeper. It
// reads the registry directly, classifies every registered workspace through
// the SINGLE owner (api.ClassifyWorkspaceOrphan), and tears down the orphans
// through the SHARED teardown owner (api.PruneWorkspace) — the same two owners
// the GUI sweeper uses, so there is no second classification or teardown path.
//
// Behavior:
//   - bare `prune`: LIST candidates, then PROMPT before tearing down.
//   - --dry-run: list + exit 0, ZERO PruneWorkspace calls (no mutation).
//   - --yes: skip the prompt (positive destructive opt-in).
//   - non-interactive shell with neither --dry-run nor --yes: REFUSE with
//     guidance (mirrors the `gui --force --kill` non-interactive posture).
//   - --idle <dur>: add the idle signal to THIS run (opts.IdleThreshold = dur)
//     WITHOUT touching the persisted daemons.prune_idle_hours setting. Absent →
//     structural orphans only (agent-worktree + deleted-dir).
//   - --json: emit an array of {PruneReport, reason} (dry-run emits the
//     candidate list with empty teardown counts).
//
// The .serena/ on-disk directory is never touched — prune is non-destructive at
// the registry level; a pruned workspace re-registers on its next open.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// pruneCandidate is one workspace the prune command classified as an orphan.
type pruneCandidate struct {
	path      string                    // canonical workspace path (registry form)
	reason    api.WorkspaceOrphanReason // why it is an orphan
	lspRows   int                       // count of per-language LSP rows for the path
	hasSerena bool                      // whether a serena (sentinel) row is present
}

// prunePlanReport is the --json element shape: the PruneWorkspace report plus
// the classification reason. On --dry-run the embedded report carries only the
// candidate identity (Workspace + a placeholder Backend) with empty teardown
// counts, because nothing was torn down.
type prunePlanReport struct {
	api.PruneReport
	Reason api.WorkspaceOrphanReason `json:"reason"`
}

// newWorkspacePruneCmd builds `mcphub workspace prune`.
func newWorkspacePruneCmd() *cobra.Command {
	var (
		dryRun  bool
		idle    time.Duration
		backend string
		yes     bool
		jsonOut bool
	)
	c := &cobra.Command{
		Use:   "prune",
		Short: "Bulk-remove registry rows for orphaned (dead) workspaces",
		Long: `Classify every registered workspace and tear down the orphans — the
manual, bulk counterpart to the in-GUI auto-prune sweeper.

A workspace is an orphan when ANY of the three shipped signals fire
(highest priority first):
  - agent-worktree: it lives inside an ephemeral .claude/worktrees/agent-*
                    worktree.
  - deleted-dir:    its directory is definitively gone (ENOENT).
  - idle:           (only with --idle) its most-recent activity is older
                    than the given duration.

Behavior:
  (bare)        List the candidates, then prompt before tearing down.
  --dry-run     List the candidates and exit; performs NO teardown.
  --yes         Skip the confirmation prompt (required in a non-interactive
                shell unless --dry-run is also passed).
  --idle <dur>  Add the idle signal to THIS run (e.g. 48h) without changing
                the persisted daemons.prune_idle_hours setting. Without it,
                only the structural signals (agent-worktree, deleted-dir) run.
  --backend     Teardown scope per orphan: all (default) | lsp | serena.
  --json        Machine-readable array of {PruneReport, reason}.

The .serena/ directory on disk is never touched; a pruned workspace
re-registers on its next open.

Examples:
  mcphub workspace prune --dry-run
  mcphub workspace prune --idle 48h --dry-run
  mcphub workspace prune --yes
  mcphub workspace prune --idle 72h --yes --json
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspacePrune(cmd, workspacePruneOpts{
				dryRun:  dryRun,
				idle:    idle,
				backend: backend,
				yes:     yes,
				jsonOut: jsonOut,
			})
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list orphan candidates and exit; perform no teardown")
	c.Flags().DurationVar(&idle, "idle", 0, "add the idle signal to this run (e.g. 48h); 0 = structural orphans only")
	c.Flags().StringVar(&backend, "backend", "all", "teardown scope per orphan: all | lsp | serena")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt (required in non-interactive shells)")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

type workspacePruneOpts struct {
	dryRun  bool
	idle    time.Duration
	backend string
	yes     bool
	jsonOut bool
}

func runWorkspacePrune(cmd *cobra.Command, opts workspacePruneOpts) error {
	switch opts.backend {
	case "all", "lsp", "serena":
	default:
		return fmt.Errorf("invalid --backend %q (want \"all\", \"lsp\", or \"serena\")", opts.backend)
	}

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	stateDir := filepath.Dir(regPath)

	// 1. Read the FULL registry (serena + every LSP row) and classify each
	//    workspace PATH through the single owner. The brief read lock is
	//    released before any teardown (PruneWorkspace takes its own locks).
	candidates, err := classifyWorkspacePruneCandidates(opts.idle)
	if err != nil {
		return err
	}

	// 2. No orphans → report and exit 0 (not an error). --json still emits an
	//    empty array so a scripted consumer always gets valid JSON.
	if len(candidates) == 0 {
		if opts.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode([]prunePlanReport{})
		}
		fmt.Fprintln(cmd.OutOrStdout(), "No orphaned workspaces found.")
		return nil
	}

	// 3. --dry-run: list (table or JSON) and exit with ZERO teardown calls.
	if opts.dryRun {
		if opts.jsonOut {
			return emitPruneDryRunJSON(cmd.OutOrStdout(), candidates, opts.backend)
		}
		printPruneCandidateTable(cmd.OutOrStdout(), candidates)
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d orphan workspace(s) (dry-run — nothing removed).\n", len(candidates))
		return nil
	}

	// 4. Confirmation gate. --yes skips it. Without --yes: a non-interactive
	//    shell REFUSES (mirrors `gui --force --kill`); an interactive shell
	//    prints the candidate table and prompts.
	if !opts.yes {
		if !inputIsTerminal(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.OutOrStderr(),
				"non-interactive shell — pass --yes to confirm teardown, or --dry-run to preview")
			return fmt.Errorf("workspace prune refused: non-interactive shell without --yes or --dry-run")
		}
		printPruneCandidateTable(cmd.OutOrStdout(), candidates)
		fmt.Fprintf(cmd.OutOrStdout(), "\nTear down %d orphan workspace(s) [y/N]: ", len(candidates))
		if !readYesNo(cmd.InOrStdin()) {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted; nothing removed.")
			return nil
		}
	}

	// 5. Apply: tear down each orphan through the SHARED owner, accumulate the
	//    reports, clear a stale default marker, and emit the result.
	reports, totalLSP, totalSerena := applyWorkspacePrune(cmd, stateDir, candidates, opts.backend)

	if opts.jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(reports)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"Pruned %d workspace(s) (%d LSP rows, %d serena rows)\n",
		len(reports), totalLSP, totalSerena)
	return nil
}

// classifyWorkspacePruneCandidates reads the full registry under a brief lock,
// groups rows by workspace path (tracking LSP-row count, serena presence, and
// most-recent activity), and returns the orphans in deterministic path order.
// It uses api.ClassifyWorkspaceOrphan — the SAME owner the GUI sweeper uses.
// idle is the per-run idle threshold (0 = structural orphans only).
func classifyWorkspacePruneCandidates(idle time.Duration) ([]pruneCandidate, error) {
	rows, err := api.NewAPI().ListAllWorkspaceRows()
	if err != nil {
		return nil, err
	}

	type agg struct {
		lspRows      int
		hasSerena    bool
		lastActivity time.Time
	}
	byPath := map[string]*agg{}
	for _, ws := range rows {
		if ws == nil || ws.WorkspacePath == "" {
			continue
		}
		a := byPath[ws.WorkspacePath]
		if a == nil {
			a = &agg{}
			byPath[ws.WorkspacePath] = a
		}
		if ws.Language == api.SerenaLanguageSentinel {
			a.hasSerena = true
		} else {
			a.lspRows++
		}
		if ws.LastToolsCallAt.After(a.lastActivity) {
			a.lastActivity = ws.LastToolsCallAt
		}
	}

	now := time.Now()
	var out []pruneCandidate
	for path, a := range byPath {
		reason, isOrphan := api.ClassifyWorkspaceOrphan(path, api.ClassifyOpts{
			IdleThreshold:   idle,
			LastToolsCallAt: a.lastActivity,
			Now:             now,
		})
		if !isOrphan {
			continue
		}
		out = append(out, pruneCandidate{
			path:      path,
			reason:    reason,
			lspRows:   a.lspRows,
			hasSerena: a.hasSerena,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// applyWorkspacePrune tears down each candidate through api.PruneWorkspace,
// clears the default marker when the pruned workspace was the persisted
// default, and returns the per-candidate {report, reason} list plus the LSP /
// serena row totals. A teardown error on one candidate is reported as a warning
// and does NOT abort the remaining candidates (bulk best-effort).
func applyWorkspacePrune(cmd *cobra.Command, stateDir string, candidates []pruneCandidate, backend string) (reports []prunePlanReport, totalLSP, totalSerena int) {
	for _, cand := range candidates {
		report, perr := api.NewAPI().PruneWorkspace(cand.path, backend)
		if perr != nil {
			fmt.Fprintf(cmd.OutOrStderr(),
				"warning: prune %s (%s) failed: %v\n", cand.path, cand.reason, perr)
			continue
		}
		if report == nil {
			continue
		}
		for _, warn := range report.Warnings {
			fmt.Fprintf(cmd.OutOrStderr(), "warning: %s\n", warn)
		}
		// Clear a stale default marker when this prune removed the serena row the
		// marker pointed at — the SAME api owner the GUI sweeper now uses.
		if report.SerenaRemoved > 0 {
			markerPath := report.Workspace
			if markerPath == "" {
				markerPath = cand.path
			}
			if cerr := api.ClearDefaultWorkspaceIfMatches(stateDir, markerPath); cerr != nil {
				fmt.Fprintf(cmd.OutOrStderr(),
					"warning: clear default marker for %s failed: %v\n", markerPath, cerr)
			}
		}
		totalLSP += len(report.LSPRemoved)
		totalSerena += report.SerenaRemoved
		reports = append(reports, prunePlanReport{PruneReport: *report, Reason: cand.reason})
	}
	return reports, totalLSP, totalSerena
}

// emitPruneDryRunJSON writes the candidate list as a JSON array of
// prunePlanReport with empty teardown counts (nothing was removed). The
// embedded report carries the candidate identity so a scripted consumer sees
// which workspaces WOULD be pruned and why.
func emitPruneDryRunJSON(w io.Writer, candidates []pruneCandidate, requestedBackend string) error {
	out := make([]prunePlanReport, 0, len(candidates))
	for _, cand := range candidates {
		out = append(out, prunePlanReport{
			PruneReport: api.PruneReport{
				Workspace:    cand.path,
				WorkspaceKey: api.WorkspaceKey(cand.path),
				// Mirror the requested --backend filter verbatim, exactly as the
				// real --yes run's PruneReport.Backend reports it (the requested
				// filter, not the effective rows removed). Keeps the dry-run JSON
				// preview byte-faithful to what the apply run would emit, instead
				// of a hasSerena-derived guess that could disagree with it.
				Backend: requestedBackend,
			},
			Reason: cand.reason,
		})
	}
	return json.NewEncoder(w).Encode(out)
}

// printPruneCandidateTable renders the WORKSPACE | REASON | LSP_ROWS | SERENA
// table the bare + --dry-run paths show.
func printPruneCandidateTable(w io.Writer, candidates []pruneCandidate) {
	fmt.Fprintf(w, "%-*s %-15s %-9s %-7s\n",
		workspaceTablePathWidth, "WORKSPACE", "REASON", "LSP_ROWS", "SERENA")
	for _, cand := range candidates {
		serena := "no"
		if cand.hasSerena {
			serena = "yes"
		}
		fmt.Fprintf(w, "%-*s %-15s %-9d %-7s\n",
			workspaceTablePathWidth,
			truncate(cand.path, workspaceTablePathWidth),
			string(cand.reason),
			cand.lspRows,
			serena)
	}
}

// readYesNo reads one line from r and reports whether it is an affirmative
// (y / yes, case-insensitive). Any other input — including EOF — is "no".
func readYesNo(r io.Reader) bool {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	resp := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return resp == "y" || resp == "yes"
}
