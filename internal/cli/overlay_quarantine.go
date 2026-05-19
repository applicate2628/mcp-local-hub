package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
)

// overlayBaseName is the canonical filename of the daemon-env overlay
// inside `<state-dir>`. Quarantine renames this file aside as
// `<base>.corrupt-<RFC3339-UTC>` so the supervisor's next cold start
// sees an absent overlay and boots with an empty effective set.
//
// Hardcoded literal per Task 2.6 spec (no exported constant exists yet
// in the daemon_env_overlay package; introducing one is out of scope).
const overlayBaseName = "daemon-env-overrides.yaml"

// overlayCorruptPrefix is the filename prefix produced by quarantine
// and matched by the 5-newest retention sweep.
const overlayCorruptPrefix = overlayBaseName + ".corrupt-"

// overlayQuarantineRetain is the number of `.corrupt-*` siblings the
// retention sweep keeps (newest by mtime). Anything older is deleted
// best-effort; per-file delete failures are non-fatal and logged.
const overlayQuarantineRetain = 5

// newOverlayQuarantineCmd builds the `mcphub config overlay-quarantine`
// subcommand. Offline only: the command reads the state-dir path via
// `stateDirFunc()`, acquires `<state-dir>/<base>.lock` via flock, and
// renames the overlay file aside. It does NOT touch the supervisor,
// IPC, or any daemon process.
func newOverlayQuarantineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "overlay-quarantine",
		Short: "Rename a corrupt daemon-env overlay aside so the supervisor can boot with empty overlay",
		Long: `Renames the daemon-env overlay file aside under a .corrupt-<timestamp>
suffix so the supervisor's next cold start (or 'mcphub restart') sees
no overlay and boots with the empty effective env set.

This is an OFFLINE recovery command: it acquires a file lock at
<state-dir>/daemon-env-overrides.yaml.lock, renames the overlay if
present, and exits. No IPC; no supervisor signal; the supervisor must
be restarted separately for the change to take effect.

After rename, keeps the 5 newest .corrupt-* siblings (by mtime) and
deletes the rest best-effort. Per-file delete failures are warned to
stderr but do not fail the command.

If the overlay file does not exist, the command prints
"no overlay to quarantine" and exits 0.

Typical operator workflow when the supervisor refuses to spawn
daemons due to a corrupt overlay:

  mcphub config overlay-quarantine
  mcphub restart`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := stateDirFunc()
			if err != nil {
				return fmt.Errorf("resolve state dir: %w", err)
			}
			return runOverlayQuarantine(stateDir, cmd.OutOrStdout())
		},
	}
}

// runOverlayQuarantine is the testable body of `overlay-quarantine`.
// `stateDir` is the directory that contains the overlay file; `out`
// receives the operator-facing message ("renamed to ..." or
// "no overlay to quarantine"). Warnings from the retention sweep are
// written to os.Stderr directly (sweep failures are not test contract).
func runOverlayQuarantine(stateDir string, out io.Writer) error {
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	lockPath := overlayPath + ".lock"

	// Acquire the overlay flock before touching the file. This serializes
	// quarantine against any concurrent supervisor read of the same path.
	// Blocking Lock (not TryLock) — quarantine is an interactive operator
	// command and waiting briefly for a competing reader is acceptable.
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("acquire overlay lock %s: %w", lockPath, err)
	}
	defer func() {
		_ = lock.Unlock()
	}()

	// Missing overlay → no-op success.
	info, statErr := os.Stat(overlayPath)
	if os.IsNotExist(statErr) {
		fmt.Fprintln(out, "no overlay to quarantine")
		return nil
	}
	if statErr != nil {
		return fmt.Errorf("stat overlay %s: %w", overlayPath, statErr)
	}
	if info.IsDir() {
		return fmt.Errorf("overlay path %s is a directory, refusing to rename", overlayPath)
	}

	// Build the .corrupt-<RFC3339-UTC> sibling name. RFC3339 contains
	// `:` characters which Windows refuses in filenames; substitute `-`
	// to make the layout cross-platform safe. UTC anchor avoids
	// locale-skew in the resulting sortable filename.
	stamp := strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339), ":", "-")
	newPath := overlayPath + ".corrupt-" + stamp

	// If a collision exists (sub-second double-invocation under flock —
	// extremely unlikely with second-resolution RFC3339, but cheap to
	// guard against), append nanosecond suffix until unique.
	if _, err := os.Stat(newPath); err == nil {
		newPath = overlayPath + ".corrupt-" + stamp + "-" +
			fmt.Sprintf("%09d", time.Now().UTC().Nanosecond())
	}

	if err := os.Rename(overlayPath, newPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", overlayPath, newPath, err)
	}

	// Best-effort retention sweep. Failures here MUST NOT mask the
	// quarantine success; emit warnings to stderr and continue.
	pruneOverlayCorruptSiblings(stateDir)

	fmt.Fprintf(out, "renamed to %s. Run 'mcphub restart' (or wait for next supervisor cold start) to apply.\n", newPath)
	return nil
}

// pruneOverlayCorruptSiblings keeps the `overlayQuarantineRetain`
// newest `.corrupt-*` siblings of the overlay file (by mtime) and
// deletes the rest. Stat / delete failures are logged warn-only and
// do not return errors — the retention sweep is best-effort.
func pruneOverlayCorruptSiblings(stateDir string) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: overlay retention sweep read-dir %s failed: %v\n", stateDir, err)
		return
	}

	type candidate struct {
		path  string
		mtime time.Time
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, overlayCorruptPrefix) {
			continue
		}
		full := filepath.Join(stateDir, name)
		info, err := os.Stat(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: overlay retention sweep stat %s failed: %v\n", full, err)
			continue
		}
		cands = append(cands, candidate{path: full, mtime: info.ModTime()})
	}

	if len(cands) <= overlayQuarantineRetain {
		return
	}

	// Newest first.
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].mtime.After(cands[j].mtime)
	})

	for _, c := range cands[overlayQuarantineRetain:] {
		if err := os.Remove(c.path); err != nil {
			fmt.Fprintf(os.Stderr, "warn: overlay retention sweep delete %s failed: %v\n", c.path, err)
		}
	}
}
