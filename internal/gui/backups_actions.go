// internal/gui/backups_actions.go
//
// Per-timestamp backup actions for the Settings → Backups section:
//
//   - POST /api/backups/restore  — overwrite a client's live config from
//     one of its recognized backup files. Snapshots the current live
//     config first (via the adapter's Backup) so a mistaken restore is
//     itself reversible.
//   - POST /api/backups/delete   — remove ONE recognized backup file.
//
// Both are DESTRUCTIVE and operate on a filesystem PATH supplied by the
// caller, so the security posture is the load-bearing part. The
// validateBackupTarget gate below pins the requested path to the
// resolving client's own config directory AND the
// `<liveName>.bak-mcp-local-hub-` naming convention, refusing anything
// else — no path traversal, no arbitrary-file write/delete. This mirrors
// the isKnownClient (400 vs 500) split already used by the clean handlers
// in backups.go.
package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

// backupActionsAPI is the narrow seam the restore/delete handlers need.
// Both methods take the ALREADY-VALIDATED absolute backup path (the
// handler runs validateBackupTarget before calling these) so the adapter
// surface stays free of path-validation duties. Behind an interface so
// the handler tests inject a fake and never touch a live client config or
// the real filesystem.
type backupActionsAPI interface {
	// Restore overwrites the named client's live config from backupPath.
	// It first snapshots the CURRENT live config to a fresh timestamped
	// backup (so a mistaken restore is itself undoable), then copies the
	// chosen backup over the live file. backupPath is pre-validated by
	// the handler. Returns the path of the safety snapshot it wrote (may
	// be empty when the client config does not yet exist), or an error.
	Restore(client, backupPath string) (snapshot string, err error)
	// Delete removes the single backup file at backupPath. backupPath is
	// pre-validated by the handler.
	Delete(backupPath string) error
}

type realBackupActionsAPI struct{}

// Restore resolves the client adapter, snapshots the current live config
// (best-effort: a fresh-install client with no live file yet returns
// ErrClientNotInstalled from Backup, which is non-fatal — there is
// nothing to lose), then routes the overwrite through the adapter's
// Restore (which itself goes through the SecureWriteClientConfig
// pipeline for JSON clients).
func (realBackupActionsAPI) Restore(client, backupPath string) (string, error) {
	c, ok := clients.AllClients()[client]
	if !ok {
		return "", fmt.Errorf("unknown client %q", client)
	}
	// Safety snapshot of the current live config before we overwrite it,
	// so the operator can undo a wrong restore. A client with no live
	// config yet (Backup returns ErrClientNotInstalled) has nothing to
	// snapshot — that is expected, not an error, so swallow only that
	// specific case and surface every other Backup failure.
	snapshot, err := c.Backup()
	if err != nil {
		var notInstalled *clients.ErrClientNotInstalled
		if !errors.As(err, &notInstalled) {
			return "", fmt.Errorf("snapshot current config before restore: %w", err)
		}
		snapshot = ""
	}
	if err := c.Restore(backupPath); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (realBackupActionsAPI) Delete(backupPath string) error {
	return os.Remove(backupPath)
}

// backupRestoreRequest / backupDeleteRequest are the JSON bodies. Both
// carry the client id (so we can resolve the authoritative config dir)
// and the absolute backup path the operator chose from the list.
type backupRestoreRequest struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

type backupDeleteRequest struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

// registerBackupsActionsRoutes wires the two per-timestamp backup action
// routes. Registered separately from registerBackupsRoutes so this lane
// owns its own file; the central server wiring just needs to call this.
func registerBackupsActionsRoutes(s *Server) {
	s.mux.HandleFunc("/api/backups/restore", s.requireSameOrigin(s.backupsRestoreHandler))
	s.mux.HandleFunc("/api/backups/delete", s.requireSameOrigin(s.backupsDeleteHandler))
}

// validateBackupTarget is the SECURITY GATE for both actions. It proves
// the (client, path) pair names a real, recognized mcp-local-hub backup
// file belonging to that client, and returns the cleaned absolute path
// to operate on.
//
// The checks, in order:
//
//  1. client must be a member of the supported-clients registry (pure
//     membership check — splits "unknown client" 400 from a runtime 500).
//  2. Resolve the client's authoritative live-config path. The ONLY
//     directory a backup may live in is filepath.Dir(configPath); the
//     ONLY filenames allowed start with `<liveBase>.bak-mcp-local-hub-`.
//  3. Clean the requested path and require that filepath.Dir(cleaned)
//     equals the expected backup dir AND filepath.Base(cleaned) carries
//     the expected prefix. filepath.Base strips any `../` traversal
//     components, and the dir-equality check pins the parent directory,
//     so a crafted `path` cannot escape the client's config dir.
//  4. Lstat the resolved path: it must exist and be a REGULAR file (no
//     symlink/reparse-point, no directory) — closes the symlink-redirect
//     hole that a plain os.Stat would follow.
//
// On any failure it returns (cleanedPath, httpStatus, code, error) with
// the caller-appropriate envelope; on success status is 0.
func validateBackupTarget(client, rawPath string) (string, int, string, error) {
	if strings.TrimSpace(client) == "" {
		return "", http.StatusBadRequest, "BACKUPS_TARGET_BAD_PARAM", fmt.Errorf("client required")
	}
	if strings.TrimSpace(rawPath) == "" {
		return "", http.StatusBadRequest, "BACKUPS_TARGET_BAD_PARAM", fmt.Errorf("path required")
	}
	if !isKnownClient(client) {
		return "", http.StatusBadRequest, "BACKUPS_TARGET_UNKNOWN_CLIENT",
			fmt.Errorf("unknown client %q (expected %s)", client, strings.Join(clients.SupportedClientNames(), " | "))
	}
	cfgPath, err := clients.ConfigPathForName(client)
	if err != nil {
		// Runtime fault resolving the config path (e.g. os.UserHomeDir
		// failure) — server-side, not an input bug.
		return "", http.StatusInternalServerError, "BACKUPS_TARGET_RESOLVE_FAILED", err
	}
	expectedDir := filepath.Dir(cfgPath)
	liveBase := filepath.Base(cfgPath)
	prefix := liveBase + ".bak-mcp-local-hub-"

	cleaned := filepath.Clean(rawPath)
	gotDir := filepath.Dir(cleaned)
	gotBase := filepath.Base(cleaned)

	// Pin both the directory and the filename convention. Comparing the
	// cleaned dir to the expected dir defeats `../` traversal; requiring
	// the backup-naming prefix defeats targeting the live config itself
	// or any other sibling file.
	if !pathsEqual(gotDir, expectedDir) || !strings.HasPrefix(gotBase, prefix) {
		return cleaned, http.StatusBadRequest, "BACKUPS_TARGET_NOT_A_BACKUP",
			fmt.Errorf("path %q is not a recognized backup for client %q", rawPath, client)
	}

	// Refuse symlinks / non-regular entries and confirm existence. Lstat
	// does NOT follow a final symlink, so an attacker-planted symlink at
	// the backup path cannot redirect the restore/delete to another file.
	fi, err := os.Lstat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return cleaned, http.StatusNotFound, "BACKUPS_TARGET_NOT_FOUND",
				fmt.Errorf("backup not found: %s", cleaned)
		}
		return cleaned, http.StatusInternalServerError, "BACKUPS_TARGET_STAT_FAILED", err
	}
	if !fi.Mode().IsRegular() {
		return cleaned, http.StatusBadRequest, "BACKUPS_TARGET_NOT_A_BACKUP",
			fmt.Errorf("backup path is not a regular file: %s", cleaned)
	}
	return cleaned, 0, "", nil
}

// pathsEqual compares two filesystem paths for equality after cleaning.
// On Windows path comparison is case-insensitive (the filesystem is), so
// the gate cannot be bypassed by case-flipping a directory component.
func pathsEqual(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if filepath.Separator == '\\' { // Windows
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (s *Server) backupsRestoreHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req backupRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	cleaned, status, code, verr := validateBackupTarget(req.Client, req.Path)
	if status != 0 {
		writeAPIError(w, verr, status, code)
		return
	}
	snapshot, err := s.backupActions.Restore(req.Client, cleaned)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_RESTORE_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"restored": cleaned,
		"client":   req.Client,
		"snapshot": snapshot, // the safety backup of the pre-restore live config ("" if none)
	})
}

func (s *Server) backupsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req backupDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	cleaned, status, code, verr := validateBackupTarget(req.Client, req.Path)
	if status != 0 {
		writeAPIError(w, verr, status, code)
		return
	}
	if strings.HasSuffix(filepath.Base(cleaned), "-original") {
		writeAPIError(
			w,
			fmt.Errorf("refusing to delete pristine original backup: %s", cleaned),
			http.StatusBadRequest,
			"BACKUPS_DELETE_ORIGINAL_FORBIDDEN",
		)
		return
	}
	if err := s.backupActions.Delete(cleaned); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_DELETE_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": cleaned,
		"client":  req.Client,
	})
}
