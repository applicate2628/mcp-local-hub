package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

// newOverlayPruneOrphansCmd builds the `mcphub config
// prune-orphan-overlay-rows` subcommand. Offline only: the command reads
// the state-dir path via `stateDirFunc()`, loads `supervisor-intent.json`
// + `daemon-env-overrides.yaml`, and rewrites the overlay with any rows
// whose taskName is NOT in the current intent removed. It does NOT
// touch the supervisor, IPC, or any daemon process.
//
// Typical operator trigger: after `mcphub unregister <workspace>` —
// the workspace's LSP daemon rows disappear from supervisor-intent.json
// but still occupy overlay slots, surfacing as
// `daemon-env-overlay-orphan-row` warnings on the next supervisor start.
func newOverlayPruneOrphansCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune-orphan-overlay-rows",
		Short: "Remove daemon-env-overrides.yaml rows whose taskName is not in supervisor-intent.json",
		Long: `Loads supervisor-intent.json and daemon-env-overrides.yaml, then
removes any overlay rows whose taskName does not match a daemon in the
current intent. Stale rows accumulate after 'mcphub unregister
<workspace>' and surface as 'daemon-env-overlay-orphan-row' warnings on
the next supervisor cold start.

This is an OFFLINE command: pure file ops, no supervisor IPC. The
supervisor picks up the cleaner overlay on its next cold start (or
'mcphub restart').

If the overlay file is missing, exits 0 with "no overlay; nothing to
prune". If no orphans are found, exits 0 with "no orphan rows; nothing
to prune" and leaves the overlay file byte-identical (no rewrite).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := stateDirFunc()
			if err != nil {
				return fmt.Errorf("resolve state dir: %w", err)
			}
			return runOverlayPruneOrphans(stateDir, cmd.OutOrStdout())
		},
	}
}

// runOverlayPruneOrphans is the testable body of `prune-orphan-overlay-rows`.
// `stateDir` is the directory containing supervisor-intent.json and the
// overlay file; `out` receives the operator-facing summary message.
//
// Behaviour matrix:
//
//   - overlay file missing       → write "no overlay; nothing to prune"; return nil.
//   - intent file missing/error  → treat as empty intent (all overlay rows
//     become orphans). The missing-intent error is logged via the message
//     but NOT propagated, so an unregister-everything operator can still
//     clean stale rows without the supervisor running.
//   - zero orphans               → write "no orphan rows; nothing to prune"; return nil
//     WITHOUT rewriting the overlay (preserves byte-for-byte identity for
//     idempotent re-runs).
//   - one or more orphans        → WriteOverlay mutator deletes orphan keys
//     and writes "Removed N orphan row(s): <comma-list>".
//
// Orphan detection normalizes both sides via daemon_env_overlay.NormalizeOverlayKey
// so an operator who hand-edited the overlay without a leading backslash
// still gets matched against the canonical-form intent.
func runOverlayPruneOrphans(stateDir string, out io.Writer) error {
	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")

	// Short-circuit on missing overlay BEFORE loading intent — the
	// supervisor may be uninstalled entirely and we shouldn't fail
	// just because intent is also absent.
	ov, loadErr := daemon_env_overlay.Load(overlayPath)
	if loadErr != nil {
		if errors.Is(loadErr, fs.ErrNotExist) {
			fmt.Fprintln(out, "no overlay; nothing to prune")
			return nil
		}
		return fmt.Errorf("load overlay %s: %w", overlayPath, loadErr)
	}
	if ov == nil || len(ov.Daemons) == 0 {
		fmt.Fprintln(out, "no overlay; nothing to prune")
		return nil
	}

	// Build the canonical-form intent task-name set. Missing intent →
	// empty set (every overlay row is an orphan), which is the correct
	// behavior for an unregister-everything cleanup.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intentSet := map[string]struct{}{}
	intent, intentErr := api.ReadSupervisorIntent(intentPath)
	if intentErr == nil && intent != nil {
		for _, d := range intent.Daemons {
			intentSet[daemon_env_overlay.NormalizeOverlayKey(d.TaskName)] = struct{}{}
		}
	}
	// Note: we deliberately swallow read errors — supervisor-uninstalled
	// or pre-install hosts have no intent file and the operator may still
	// want to clean a stale overlay. The empty intent set → every overlay
	// row classified as orphan, which is the desired behaviour here.

	// Collect orphan keys. Iterate overlay map in stable sort order so the
	// operator-visible "Removed N: ..." message is deterministic across runs.
	overlayKeys := make([]string, 0, len(ov.Daemons))
	for k := range ov.Daemons {
		overlayKeys = append(overlayKeys, k)
	}
	sort.Strings(overlayKeys)

	var orphans []string
	for _, k := range overlayKeys {
		if _, kept := intentSet[daemon_env_overlay.NormalizeOverlayKey(k)]; !kept {
			orphans = append(orphans, k)
		}
	}

	if len(orphans) == 0 {
		fmt.Fprintln(out, "no orphan rows; nothing to prune")
		return nil
	}

	// Rewrite overlay under the WriteOverlay flock with the orphans removed.
	err := daemon_env_overlay.WriteOverlay(overlayPath, func(o *daemon_env_overlay.Overlay) error {
		for _, k := range orphans {
			delete(o.Daemons, k)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prune orphans from overlay %s: %w", overlayPath, err)
	}

	fmt.Fprintf(out, "Removed %d orphan row(s): %s\n", len(orphans), strings.Join(orphans, ", "))
	return nil
}
