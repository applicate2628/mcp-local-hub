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

// DefaultMarkerCompensationOutcome is the committed result of a stale
// workspace-register compensation attempt. It is intentionally distinct from
// the older ClearDefaultWorkspaceIfMatches API, whose established callers only
// need the error contract.
type DefaultMarkerCompensationOutcome string

const (
	// DefaultMarkerCompensationCleared means the expected stale marker was
	// still present and was cleared while the registry lock was held.
	DefaultMarkerCompensationCleared DefaultMarkerCompensationOutcome = "cleared"
	// DefaultMarkerCompensationPreservedRegistrationPresent means a current
	// Serena registration owns the canonical path, so its marker must survive.
	DefaultMarkerCompensationPreservedRegistrationPresent DefaultMarkerCompensationOutcome = "preserved_registration_present"
	// DefaultMarkerCompensationPreservedMarkerChanged means the marker no
	// longer named the stale registration, so it was left untouched.
	DefaultMarkerCompensationPreservedMarkerChanged DefaultMarkerCompensationOutcome = "preserved_marker_changed"
)

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
	_, err := clearDefaultWorkspaceIfMatches(stateDir, canonical)
	return err
}

// clearDefaultWorkspaceIfMatches is the marker-only compare-and-clear
// primitive. Callers that also own a registry decision use its committed
// outcome to report whether the marker was actually cleared.
func clearDefaultWorkspaceIfMatches(stateDir, canonical string) (DefaultMarkerCompensationOutcome, error) {
	return clearDefaultWorkspaceIfMatchesWithMarkerLockHook(stateDir, canonical, nil)
}

func clearDefaultWorkspaceIfMatchesWithMarkerLockHook(stateDir, canonical string, markerLockHeld func()) (DefaultMarkerCompensationOutcome, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir default workspace state dir: %w", err)
	}
	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	lockPath := path + ".lock"
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return "", fmt.Errorf("default-workspace flock %s: %w", lockPath, err)
	}
	defer func() { _ = lock.Unlock() }()
	if markerLockHeld != nil {
		markerLockHeld()
	}

	got, err := ReadDefaultWorkspace(stateDir)
	if err != nil {
		return "", err
	}
	if got != canonical {
		return DefaultMarkerCompensationPreservedMarkerChanged, nil
	}
	if err := WriteStateFileBytesLockHeld(path, []byte("")); err != nil {
		return "", err
	}
	return DefaultMarkerCompensationCleared, nil
}

// defaultMarkerCompensationMarkerLockHeldFn is a package-private test seam
// used only to prove that the registry lock remains held until the marker CAS
// completes. Production leaves it as a no-op.
var defaultMarkerCompensationMarkerLockHeldFn = func() {}

// ClearDefaultWorkspaceForAbsentSerenaRegistration compensates a stale
// --default marker only after a FRESH registry read confirms that the old
// Serena registration is absent. It locks registry -> marker, the same order
// used by workspace registration, so a newer same-path registration cannot
// publish its row and marker between this absence check and the marker clear.
//
// workspaceKey and canonical are the old operation's immutable registration
// identity. A row under workspaceKey with a different canonical path is an
// integrity contradiction and fails closed. A row under any key with the same
// canonical path is a current registration and preserves the marker.
func ClearDefaultWorkspaceForAbsentSerenaRegistration(registryPath, workspaceKey, canonical string) (DefaultMarkerCompensationOutcome, error) {
	reg := NewRegistry(registryPath)
	unlock, err := reg.Lock()
	if err != nil {
		return "", fmt.Errorf("lock registry before default marker compensation: %w", err)
	}
	defer unlock()

	if err := reg.Load(); err != nil {
		return "", fmt.Errorf("load registry before default marker compensation: %w", err)
	}
	if entry, ok := reg.GetSerena(workspaceKey); ok {
		if entry.WorkspacePath != canonical {
			return "", fmt.Errorf("default marker compensation registry identity conflict: workspace key %q maps to %q, not expected canonical path %q", workspaceKey, entry.WorkspacePath, canonical)
		}
		return DefaultMarkerCompensationPreservedRegistrationPresent, nil
	}
	for _, entry := range reg.SerenaEntries() {
		if entry.WorkspacePath == canonical {
			return DefaultMarkerCompensationPreservedRegistrationPresent, nil
		}
	}

	// Keep the registry lock through the marker CAS. A concurrent registration
	// takes this same registry lock before publishing its row and marker.
	return clearDefaultWorkspaceIfMatchesWithMarkerLockHook(filepath.Dir(registryPath), canonical, defaultMarkerCompensationMarkerLockHeldFn)
}
