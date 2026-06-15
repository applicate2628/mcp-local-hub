// internal/gui/backups.go
package gui

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// isKnownClient reports whether name is a member of the supported-
// clients registry. Pure membership check — does NOT invoke
// ConfigPathForName so it can't return a runtime error. Used by the
// per-client backups handlers to split "input bug" (400 unknown-client)
// from "server fault" (500 runtime failure). Bot r2 P2 closure on
// PR #183.
func isKnownClient(name string) bool {
	return slices.Contains(clients.SupportedClientNames(), name)
}

// backupsAPI is the narrow surface used by /api/backups handlers.
// CleanInClient + CleanPreviewInClient narrow the prune set to a single
// client (bug-bash B2 closure #21: pre-fix, the GUI had one global
// "Clean now" button that pruned every client's backups — operators
// who only wanted to clean cursor's backups had no way to scope it).
type backupsAPI interface {
	List() ([]api.BackupInfo, error)
	CleanPreview(keepN int) ([]string, error)
	Clean(keepN int) ([]string, error)
	CleanPreviewInClient(client string, keepN int) ([]string, error)
	CleanInClient(client string, keepN int) ([]string, error)
}

type realBackupsAPI struct{}

func (realBackupsAPI) List() ([]api.BackupInfo, error)      { return api.NewAPI().BackupsList() }
func (realBackupsAPI) CleanPreview(n int) ([]string, error) { return api.NewAPI().BackupsCleanPreview(n) }
func (realBackupsAPI) Clean(n int) ([]string, error)        { return api.NewAPI().BackupsClean(n) }

// CleanPreviewInClient resolves the client's config path and previews
// what BackupsCleanIn(dir, basename, keepN) would prune (dryRun=true
// inside BackupsCleanIn would require a flag the API doesn't expose;
// the api.BackupsCleanPreview helper iterates ALL clients, so for the
// per-client preview we use BackupsListIn + the same retention math
// the bulk path uses).
func (realBackupsAPI) CleanPreviewInClient(client string, keepN int) ([]string, error) {
	if keepN < 0 {
		return nil, fmt.Errorf("keepN must be >= 0")
	}
	cfgPath, err := clients.ConfigPathForName(client)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfgPath)
	live := filepath.Base(cfgPath)
	rows, err := api.NewAPI().BackupsListIn(dir, live)
	if err != nil {
		return nil, err
	}
	return previewTimestampedRetention(rows, keepN), nil
}

// CleanInClient delegates to api.BackupsCleanIn after resolving the
// client's canonical config path. Returns the slice of deleted paths.
func (realBackupsAPI) CleanInClient(client string, keepN int) ([]string, error) {
	cfgPath, err := clients.ConfigPathForName(client)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfgPath)
	live := filepath.Base(cfgPath)
	return api.NewAPI().BackupsCleanIn(dir, live, keepN)
}

// previewTimestampedRetention computes the would-be-pruned paths from
// a per-client BackupInfo list. Mirrors the retention math in
// api.backupsCleanInImpl: sort timestamped backups newest-first, keep
// the first keepN, return the rest. Sentinels never appear in the
// result. Pure helper for testability — no filesystem side effects.
func previewTimestampedRetention(rows []api.BackupInfo, keepN int) []string {
	type ts struct {
		path    string
		modTime time.Time
	}
	var stamped []ts
	for _, r := range rows {
		if r.Kind != "timestamped" {
			continue
		}
		stamped = append(stamped, ts{path: r.Path, modTime: r.ModTime})
	}
	if len(stamped) <= keepN {
		return []string{}
	}
	// Newest first (same as api.backupsCleanInImpl).
	for i := 1; i < len(stamped); i++ {
		for j := i; j > 0 && stamped[j].modTime.After(stamped[j-1].modTime); j-- {
			stamped[j], stamped[j-1] = stamped[j-1], stamped[j]
		}
	}
	out := make([]string, 0, len(stamped)-keepN)
	for _, b := range stamped[keepN:] {
		out = append(out, b.path)
	}
	return out
}

// backupDTO is the JSON shape of one entry in GET /api/backups.
// ModTime is serialized as RFC3339 for predictable wire format.
type backupDTO struct {
	Client   string `json:"client"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "original" | "timestamped"
	ModTime  string `json:"mod_time"`
	SizeByte int64  `json:"size_byte"`
}

func registerBackupsRoutes(s *Server) {
	s.mux.HandleFunc("/api/backups", s.requireSameOrigin(s.backupsListHandler))
	s.mux.HandleFunc("/api/backups/clean-preview", s.requireSameOrigin(s.backupsCleanPreviewHandler))
	s.mux.HandleFunc("/api/backups/clean", s.requireSameOrigin(s.backupsCleanHandler))
}

func (s *Server) backupsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rows, err := s.backups.List()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_LIST_FAILED")
		return
	}
	dtos := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		dtos = append(dtos, backupDTO{
			Client:   b.Client,
			Path:     b.Path,
			Kind:     b.Kind,
			ModTime:  b.ModTime.Format(time.RFC3339),
			SizeByte: b.SizeByte,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": dtos})
}

func (s *Server) backupsCleanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Read the user's persisted keep_n from settings; fall back to 5 (registry
	// default) if missing, unset, or unparseable — same guard used throughout
	// the api package for this setting.
	keepN := 5
	if s, err := api.NewAPI().SettingsGet("backups.keep_n"); err == nil && s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			keepN = n
		}
	}
	// Explicit ?keep_n=N query param overrides the persisted setting. This is
	// how the GUI's "Clean now" deletes exactly what its live preview showed:
	// the preview (GET /api/backups/clean-preview?keep_n=N) and the clean both
	// use the slider's DRAFT value, so the action is WYSIWYG even when the user
	// hasn't pressed Save. Pre-fix the preview used the draft but the clean read
	// the persisted setting, so dragging the slider without saving showed an
	// eligible count the clean then refused to act on ("Clean X only (3)" →
	// cleaned:0). Absent → persisted fallback above (backward compat for API
	// callers). Invalid → 400; never silently fall back to a destructive default
	// (destructive-default polarity discipline).
	if q := r.URL.Query().Get("keep_n"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			writeAPIError(w, fmt.Errorf("keep_n must be a non-negative integer"), http.StatusBadRequest, "BACKUPS_CLEAN_BAD_PARAM")
			return
		}
		keepN = n
	}
	// Bug-bash B2 closure (#21): optional ?client=X narrows the prune
	// to one client. Empty client preserves the legacy "clean every
	// managed client" semantic so existing operator workflows keep
	// working unchanged.
	//
	// Bot r1 P2 + r2 P2 closure (PR #183): split client-validation
	// (400) from runtime/filesystem failure (500). Use a pure
	// registry-membership check via clients.SupportedClientNames so
	// we don't conflate "client name not in registry" (input bug,
	// 400) with "ConfigPathForName had a runtime failure such as
	// os.UserHomeDir() returning ENOENT" (server-side fault, 500).
	// Any subsequent error from CleanInClient (including its own
	// ConfigPathForName call hitting that same runtime failure) lands
	// in the 500 BACKUPS_CLEAN_FAILED branch below.
	client := r.URL.Query().Get("client")
	var removed []string
	var err error
	if client != "" {
		if !isKnownClient(client) {
			writeAPIError(w,
				fmt.Errorf("unknown client %q (expected %s)", client, strings.Join(clients.SupportedClientNames(), " | ")),
				http.StatusBadRequest, "BACKUPS_CLEAN_UNKNOWN_CLIENT")
			return
		}
		removed, err = s.backups.CleanInClient(client, keepN)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_CLEAN_FAILED")
			return
		}
	} else {
		removed, err = s.backups.Clean(keepN)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_CLEAN_FAILED")
			return
		}
	}
	if removed == nil {
		removed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cleaned": len(removed),
		"client":  client,
		"errors":  []string{},
	})
}

func (s *Server) backupsCleanPreviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query().Get("keep_n")
	if q == "" {
		writeAPIError(w, fmt.Errorf("missing keep_n"), http.StatusBadRequest, "BACKUPS_PREVIEW_BAD_PARAM")
		return
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 0 {
		writeAPIError(w, fmt.Errorf("keep_n must be a non-negative integer"), http.StatusBadRequest, "BACKUPS_PREVIEW_BAD_PARAM")
		return
	}
	// Bug-bash B2 closure (#21) symmetry: optional ?client=X narrows
	// the preview to one client so per-client "would-prune" counts
	// can render alongside each client's backups group.
	//
	// Bot r1 P2 + r2 P2 closure (PR #183) symmetry: pure registry-
	// membership check via isKnownClient (no ConfigPathForName runtime
	// surface) splits "unknown-client" 400 from "I/O failure on a
	// known client" 500.
	client := r.URL.Query().Get("client")
	var paths []string
	if client != "" {
		if !isKnownClient(client) {
			writeAPIError(w,
				fmt.Errorf("unknown client %q (expected %s)", client, strings.Join(clients.SupportedClientNames(), " | ")),
				http.StatusBadRequest, "BACKUPS_PREVIEW_UNKNOWN_CLIENT")
			return
		}
		paths, err = s.backups.CleanPreviewInClient(client, n)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_PREVIEW_FAILED")
			return
		}
	} else {
		paths, err = s.backups.CleanPreview(n)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_PREVIEW_FAILED")
			return
		}
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"would_remove": paths, "client": client})
}
