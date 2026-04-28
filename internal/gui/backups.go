// internal/gui/backups.go
package gui

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"mcp-local-hub/internal/api"
)

// backupsAPI is the narrow surface used by /api/backups handlers.
type backupsAPI interface {
	List() ([]api.BackupInfo, error)
	CleanPreview(keepN int) ([]string, error)
}

type realBackupsAPI struct{}

func (realBackupsAPI) List() ([]api.BackupInfo, error)      { return api.NewAPI().BackupsList() }
func (realBackupsAPI) CleanPreview(n int) ([]string, error) { return api.NewAPI().BackupsCleanPreview(n) }

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
	paths, err := s.backups.CleanPreview(n)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_PREVIEW_FAILED")
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"would_remove": paths})
}
