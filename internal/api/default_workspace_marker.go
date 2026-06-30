// internal/api/default_workspace_marker.go
//
// Single owner of the `default-workspace.txt` sidecar marker that records the
// operator-selected default serena workspace by its canonical path.
//
// The marker logic previously lived ONLY in internal/cli/workspace_cmd.go, so
// the GUI auto-prune sweeper (internal/gui) — which CANNOT import internal/cli
// (the dependency direction is cli → gui → api) — had no way to clear a stale
// default when a prune removed the serena row the marker pointed at. That was a
// real correctness gap: a sweep that tore down the default workspace's serena
// row left the marker dangling at a path with no live registration.
//
// Lifting the read / write / clear-if-matches helpers into internal/api (the
// lowest module both cli and gui depend on) gives BOTH surfaces ONE owner. The
// CLI's own writeDefaultWorkspace / readDefaultWorkspace / clearDefaultIfMatches
// helpers re-point here so there is a single maintained implementation (no logic
// duplication). The marker file path is resolved EXACTLY as before — the caller
// passes the stateDir (filepath.Dir of the registry path) so the marker stays
// co-located with workspaces.yaml and the on-disk location does not move.

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// DefaultWorkspaceFilename is the sidecar file (alongside workspaces.yaml) that
// records the operator-selected default serena workspace by its canonical path.
// Absent file = no default. Empty file = no default.
const DefaultWorkspaceFilename = "default-workspace.txt"

// WriteDefaultWorkspace persists the canonical default workspace path under
// stateDir (or an empty string to clear). Atomic rename so a crash mid-write
// cannot leave a truncated file.
func WriteDefaultWorkspace(stateDir, canonical string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	return WriteStateFileBytesAtomic(path, []byte(canonical))
}

// ReadDefaultWorkspace returns the persisted default workspace path under
// stateDir, or the empty string when the marker file is absent or empty.
func ReadDefaultWorkspace(stateDir string) (string, error) {
	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	data, err := ReadStateFileInodeAnchored(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ClearDefaultWorkspaceIfMatches removes the marker when its stored value
// equals canonical. Returns nil if the marker is absent OR points elsewhere
// (a non-matching default must survive). This is the side effect both
// `workspace unregister --backend serena|all` and the GUI auto-prune sweeper
// run so a stale default cannot outlive the workspace it pointed at.
func ClearDefaultWorkspaceIfMatches(stateDir, canonical string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir default workspace state dir: %w", err)
	}
	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	lockPath := path + ".lock"
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("default-workspace flock %s: %w", lockPath, err)
	}
	defer func() { _ = lock.Unlock() }()

	got, err := ReadDefaultWorkspace(stateDir)
	if err != nil {
		return err
	}
	if got != canonical {
		return nil
	}
	return WriteStateFileBytesLockHeld(path, []byte(""))
}
