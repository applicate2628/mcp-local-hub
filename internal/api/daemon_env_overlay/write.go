// write.go — WriteOverlay flock-protected read-modify-write helper
// (Task 2.3 of the v0.5.x Servers matrix revamp).
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Apply env edit from GUI" + acceptance criteria I-V4-4.
//
// WriteOverlay serializes concurrent mutators on a per-file flock and
// hands each one an Overlay loaded from disk (or empty if the file is
// missing). The mutator returns either the same overlay with edits
// applied or an error; on error WriteOverlay propagates the error
// verbatim and DOES NOT touch the on-disk file. On success the
// overlay is marshalled to YAML and routed through
// api.WriteStateFileBytesLockHeld for atomic temp+rename + DACL
// handle-binding under the state-file policy domain (matches the
// supervisor-intent/state write path so the new file stays owner-only
// while keeping state-file relax/audit semantics).
//
// Known limitation — YAML comments are NOT preserved. The marshal
// path here is struct-based (yaml.Marshal on *Overlay), which drops
// any comments an operator authored manually in the overlay file. The
// operator's typical workflow is GUI-driven so this is acceptable;
// if comment preservation later becomes a hard requirement, the
// implementation can be upgraded to use yaml.v3's Node API.

package daemon_env_overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
)

// WriteOverlay performs a flock-protected read-modify-write on the
// overlay file at path:
//
//  1. Ensure path's parent directory exists (MkdirAll 0o700).
//  2. Acquire a blocking flock on "<path>.lock". Concurrent callers
//     serialize here.
//  3. Load the existing overlay (Load returns an empty Overlay when
//     the file is missing, so first-write callers see the same
//     non-nil overlay shape as subsequent callers).
//  4. Invoke mutator(overlay). The mutator edits the in-memory
//     overlay; it returns nil on success or an error to abort.
//  5. If mutator returns a non-nil error, WriteOverlay releases the
//     flock and returns that error verbatim. The on-disk file is
//     NOT touched — the mutator's in-memory edits never reach disk.
//  6. Otherwise, marshal the (possibly-mutated) overlay to YAML and
//     write it via api.WriteStateFileBytesLockHeld (state-file
//     atomic temp+rename + handle-bound DACL on Windows / 0600 on
//     POSIX).
//  7. Release the flock (deferred — fires on any return path).
//
// Contract details:
//   - The flock lockfile is "<path>.lock", NOT the overlay file
//     itself. Callers may safely os.Remove(path) without disturbing
//     in-flight lock acquisition (the lockfile lives beside it).
//   - mutator receives a non-nil *Overlay with a non-nil Daemons
//     map (Load guarantees the latter for both missing-file and
//     present-file paths).
//   - Comments authored manually by operators in the overlay file
//     are lost on next WriteOverlay (yaml.Marshal is struct-based).
//     See the package-doc note above for the rationale.
func WriteOverlay(path string, mutator func(*Overlay) error) error {
	if mutator == nil {
		return errors.New("daemon_env_overlay.WriteOverlay: mutator is nil")
	}

	// Ensure the parent directory exists before flock acquisition
	// (flock.New requires the lockfile's parent to exist; on first
	// write the operator's state-dir may not have the overlay's
	// parent yet). 0o700 matches the rest of the per-user state-dir
	// posture documented in CLAUDE.md "State path" §.
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("daemon_env_overlay.WriteOverlay: mkdir parent %q: %w", parent, err)
	}

	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("daemon_env_overlay.WriteOverlay: acquire flock %q: %w", lockPath, err)
	}
	// Deferred unlock fires on any return path, including the
	// mutator-error rollback below — the lockfile is never leaked.
	defer func() { _ = lk.Unlock() }()

	overlay, err := Load(path)
	if err != nil {
		return fmt.Errorf("daemon_env_overlay.WriteOverlay: load %q: %w", path, err)
	}

	if err := mutator(overlay); err != nil {
		// Rollback contract: on mutator error the on-disk file
		// remains untouched. Propagate the error verbatim so
		// callers can errors.Is against their sentinels.
		return err
	}

	raw, err := yaml.Marshal(overlay)
	if err != nil {
		return fmt.Errorf("daemon_env_overlay.WriteOverlay: marshal %q: %w", path, err)
	}

	if err := api.WriteStateFileBytesLockHeld(path, raw); err != nil {
		return fmt.Errorf("daemon_env_overlay.WriteOverlay: secure write %q: %w", path, err)
	}
	return nil
}
