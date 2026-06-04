// internal/gui/workspaces.go
//
// GET /api/workspaces — read-only surface over the workspace registry
// at workspaces.yaml. Feeds the v0.5.x Servers-matrix WorkspaceSelector
// so the operator can scope LSP rows to one workspace's task set.
//
// Two-tier shape on the wire:
//
//   - `workspaces`: deduplicated [{ workspace_key, workspace_path }]
//     pairs sorted by key. The selector picks one of these.
//   - `entries`:    full WorkspaceEntry list (every (key, language)
//     tuple). The matrix uses this to discover the per-language
//     task_name for each registered LSP, including ownership keys
//     (`mcp-language-server-<lang>-<suffix>` after ResolveEntryName).
//
// A missing registry file is not an error — both arrays come back
// empty so the frontend can render the "(none — register a workspace
// first)" placeholder without a try/catch.
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"

	"mcp-local-hub/internal/api"
)

// workspacePair is one (key, path) entry in the deduplicated list.
type workspacePair struct {
	WorkspaceKey  string `json:"workspace_key"`
	WorkspacePath string `json:"workspace_path"`
}

// workspaceEntryDTO mirrors api.WorkspaceEntry but with JSON tags so
// the frontend doesn't need to know about the YAML-only field names.
// Lifecycle / LastError stay omitempty so an unused legacy entry
// reads identically on the wire to a freshly-registered one.
type workspaceEntryDTO struct {
	WorkspaceKey  string            `json:"workspace_key"`
	WorkspacePath string            `json:"workspace_path"`
	Language      string            `json:"language"`
	Backend       string            `json:"backend"`
	Port          int               `json:"port"`
	TaskName      string            `json:"task_name"`
	ClientEntries map[string]string `json:"client_entries,omitempty"`
	Lifecycle     string            `json:"lifecycle,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
}

func workspaceEntryDTOFromAPI(ws api.WorkspaceEntry) workspaceEntryDTO {
	return workspaceEntryDTO{
		WorkspaceKey:  ws.WorkspaceKey,
		WorkspacePath: ws.WorkspacePath,
		Language:      ws.Language,
		Backend:       ws.Backend,
		Port:          ws.Port,
		TaskName:      ws.TaskName,
		ClientEntries: ws.ClientEntries,
		Lifecycle:     ws.Lifecycle,
		LastError:     ws.LastError,
	}
}

type workspacesResponse struct {
	Workspaces []workspacePair     `json:"workspaces"`
	Entries    []workspaceEntryDTO `json:"entries"`
}

func registerWorkspacesRoutes(s *Server) {
	s.mux.HandleFunc("/api/workspaces", s.requireSameOrigin(s.workspacesHandler))
}

// workspacesTestSeam lets tests inject a synthetic registry without
// touching disk. Production path is loadWorkspaceRegistryProd.
var workspacesTestSeam func() (*api.Registry, error)

func loadWorkspaceRegistryProd() (*api.Registry, error) {
	if workspacesTestSeam != nil {
		return workspacesTestSeam()
	}
	path, err := api.DefaultRegistryPath()
	if err != nil {
		return nil, err
	}
	reg := api.NewRegistry(path)
	if err := reg.Load(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Load already maps IsNotExist to empty-registry success,
			// but keep this guard as defense-in-depth in case Load's
			// contract is tightened later.
			return reg, nil
		}
		return nil, err
	}
	return reg, nil
}

func (s *Server) workspacesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg, err := loadWorkspaceRegistryProd()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "REGISTRY_LOAD_FAILED")
		return
	}

	seen := map[string]workspacePair{}
	entries := make([]workspaceEntryDTO, 0, len(reg.Workspaces))
	for _, ws := range reg.Workspaces {
		if _, dup := seen[ws.WorkspaceKey]; !dup {
			seen[ws.WorkspaceKey] = workspacePair{
				WorkspaceKey:  ws.WorkspaceKey,
				WorkspacePath: ws.WorkspacePath,
			}
		}
		entries = append(entries, workspaceEntryDTOFromAPI(ws))
	}

	pairs := make([]workspacePair, 0, len(seen))
	for _, p := range seen {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].WorkspaceKey < pairs[j].WorkspaceKey
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].WorkspaceKey != entries[j].WorkspaceKey {
			return entries[i].WorkspaceKey < entries[j].WorkspaceKey
		}
		return entries[i].Language < entries[j].Language
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(workspacesResponse{
		Workspaces: pairs,
		Entries:    entries,
	}); err != nil {
		http.Error(w, "workspaces encode failed", http.StatusInternalServerError)
	}
}
