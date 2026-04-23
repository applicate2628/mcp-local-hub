// internal/api/dismiss.go
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// dismissedFileName is the per-machine persistence file for the
// Migration screen's "Dismiss" action on unknown stdio entries. Lives
// alongside other per-machine state (logs, settings, workspaces) so
// GUI and CLI agree on the same directory root.
const dismissedFileName = "gui-dismissed.json"

// dismissMu serializes concurrent DismissUnknown calls. Two racing
// POSTs could otherwise read the old file, add their name, and race
// the rename — losing one of the two dismissals.
var dismissMu sync.Mutex

// dismissedPayload is the on-disk JSON shape.
//
// Versioned via the top-level "version" field so a future A4 Settings
// schema change (e.g. "dismissed with timestamp", "per-client
// dismissal") can migrate without silently losing entries. Current
// version is 1; readers that encounter an unknown version fail
// closed (empty set returned) so the user can re-dismiss rather than
// seeing silently-half-applied state.
type dismissedPayload struct {
	Version int      `json:"version"`
	Unknown []string `json:"unknown"`
}

// ListDismissedUnknown returns the set of server names the user has
// dismissed in the Migration screen. Caller receives a map for O(1)
// membership checks (convert to slice with a helper if needed).
//
// Returns an empty set + nil error in all soft-failure cases:
// missing file, corrupt JSON, wrong schema version. Hard errors
// (permission denied, unreadable dir) surface so the GUI handler
// can log them without blocking the screen render.
func ListDismissedUnknown() (map[string]struct{}, error) {
	path, err := dismissedFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]struct{}{}, nil
	}
	var payload dismissedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		// Corrupt file: return empty rather than blocking. Next
		// DismissUnknown write will overwrite the corrupt content.
		return map[string]struct{}{}, nil
	}
	if payload.Version != 1 {
		// Unknown schema version: same fail-closed rationale as
		// corrupt JSON — prefer an empty set over a silently
		// partially-honored list.
		return map[string]struct{}{}, nil
	}
	out := make(map[string]struct{}, len(payload.Unknown))
	for _, name := range payload.Unknown {
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	return out, nil
}

// DismissUnknown marks a server name as dismissed, persisting
// atomically (temp file + rename) so a crash mid-write can never
// leave a truncated on-disk list. Idempotent: dismissing the same
// name twice is a no-op.
//
// Returns an error on empty name (caller bug), filesystem failure
// (disk full, permission denied), or atomicity violation (rename
// failed). Dismissing a name that is currently in the hub-HTTP /
// can-migrate / per-session state is NOT rejected — the stored list
// only affects rendering in the "Unknown" group, so dismissing a
// name that later transitions to a different status is harmless:
// the name sits in the file unused until the transition flips back.
func DismissUnknown(name string) error {
	if name == "" {
		return errors.New("DismissUnknown: name must not be empty")
	}
	dismissMu.Lock()
	defer dismissMu.Unlock()

	existing, err := ListDismissedUnknown()
	if err != nil {
		return err
	}
	if _, already := existing[name]; already {
		return nil
	}
	existing[name] = struct{}{}

	sorted := make([]string, 0, len(existing))
	for n := range existing {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted) // Stable on-disk order for readable diffs / grep.

	payload := dismissedPayload{Version: 1, Unknown: sorted}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dismissed payload: %w", err)
	}

	path, err := dismissedFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	// Atomic write: write to sibling temp file, then rename over the
	// target. Rename is atomic on both NTFS and ext4 for same-directory
	// renames; the sibling lives in the same dir as the target so the
	// cross-device-link gotcha does not apply.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // Best-effort cleanup on rename failure.
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// dismissedFilePath resolves the per-machine storage path using the
// same state-directory convention as internal/api/workspace_registry.go:71
// and internal/api/logs.go:93:
//   - Windows: %LOCALAPPDATA%\mcp-local-hub\gui-dismissed.json
//   - POSIX:   $XDG_STATE_HOME/mcp-local-hub/gui-dismissed.json
//     (~/.local/state/mcp-local-hub/gui-dismissed.json fallback)
//
// Dismissed data is transient state (user toggled "don't show me this
// again"), not user config data — state-dir is the right place.
// LOCALAPPDATA is checked first on Windows; POSIX prefers XDG_STATE_HOME
// then falls back to ~/.local/state. Tests override via
// t.Setenv("LOCALAPPDATA", t.TempDir()) (install_test.go:287 pattern).
func dismissedFilePath() (string, error) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		// Honored on non-Windows too so the e2e hub fixture (which
		// sets LOCALAPPDATA: home to redirect every state path —
		// see internal/gui/e2e/fixtures/hub.ts:46) keeps working on
		// a Linux CI runner without extra plumbing.
		return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", dismissedFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub", dismissedFileName), nil
}
