package gui

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type adoptRequest struct {
	Entry        string   `json:"entry"`
	Client       string   `json:"client"`
	Clients      []string `json:"clients"`
	Name         string   `json:"name"`
	Port         int      `json:"port"`
	AllowSymlink bool     `json:"allow_symlink"`
}

type adoptSymlinkTarget struct {
	Client       string `json:"client"`
	ResolvedPath string `json:"resolved_path"`

	originalPath string
	pinnedPath   string
}

type adoptPlanResponse struct {
	*api.AdoptPlan
	SymlinkTargets []adoptSymlinkTarget `json:"symlink_targets"`
}

type adoptExecuteResponse struct {
	Name           string               `json:"name"`
	Port           int                  `json:"port"`
	AdoptClients   []string             `json:"adopt_clients"`
	SymlinkTargets []adoptSymlinkTarget `json:"symlink_targets"`
}

var adoptSymlinkConsentMu sync.Mutex

func registerAdoptRoutes(s *Server) {
	s.mux.HandleFunc("/api/adopt/plan", s.requireSameOrigin(s.adoptPlanHandler))
	s.mux.HandleFunc("/api/adopt", s.requireSameOrigin(s.adoptHandler))
}

func (s *Server) adoptPlanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	plan, targets, err := buildGUIAdoptPlan(&req)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adoptPlanResponse{
		AdoptPlan:      plan,
		SymlinkTargets: targets,
	})
}

func (s *Server) adoptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adoptRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	plan, targets, err := buildGUIAdoptPlan(&req)
	if err != nil {
		writeAdoptPlanError(w, err)
		return
	}
	if len(targets) > 0 && !req.AllowSymlink {
		writeAPIError(w, fmt.Errorf("symlink consent required for %s", formatAdoptSymlinkTargets(targets)), http.StatusConflict, "SYMLINK_CONSENT_REQUIRED")
		return
	}

	restoreConsent := func() {}
	if len(targets) > 0 {
		restoreConsent = installAdoptSymlinkConsent(targets)
	}
	defer restoreConsent()

	if err := api.NewAPI().ExecuteAdopt(plan, io.Discard); err != nil {
		status, code := adoptExecuteErrorStatus(err)
		writeAPIError(w, err, status, code)
		return
	}
	writeJSON(w, http.StatusCreated, adoptExecuteResponse{
		Name:           plan.ManifestName,
		Port:           plan.Port,
		AdoptClients:   plan.AdoptClients,
		SymlinkTargets: targets,
	})
}

func buildGUIAdoptPlan(req *adoptRequest) (*api.AdoptPlan, []adoptSymlinkTarget, error) {
	plan, err := api.NewAPI().BuildAdoptPlan(api.AdoptOpts{
		EntryName:    req.Entry,
		Client:       req.Client,
		ManifestName: req.Name,
		Port:         req.Port,
		Clients:      req.Clients,
	})
	if err != nil {
		return nil, nil, err
	}
	targets, err := adoptSymlinkTargets(plan.AdoptClients)
	if err != nil {
		return nil, nil, err
	}
	return plan, targets, nil
}

func adoptSymlinkTargets(clientNames []string) ([]adoptSymlinkTarget, error) {
	targets := make([]adoptSymlinkTarget, 0)
	for _, client := range clientNames {
		configPath, err := clients.ConfigPathForName(client)
		if err != nil {
			return nil, fmt.Errorf("resolve client config path for %q: %w", client, err)
		}
		resolved, pinned, isSymlink := api.ResolveClientConfigSymlink(configPath)
		if !isSymlink {
			continue
		}
		targets = append(targets, adoptSymlinkTarget{
			Client:       client,
			ResolvedPath: resolved,
			originalPath: filepath.Clean(configPath),
			pinnedPath:   filepath.Clean(pinned),
		})
	}
	return targets, nil
}

func writeAdoptPlanError(w http.ResponseWriter, err error) {
	status, code := adoptPlanErrorStatus(err)
	writeAPIError(w, err, status, code)
}

func adoptPlanErrorStatus(err error) (int, string) {
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	return http.StatusBadRequest, "BAD_INPUT"
}

func adoptExecuteErrorStatus(err error) (int, string) {
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "collides with a shipped") {
		return http.StatusConflict, "NAME_CONFLICT"
	}
	if strings.Contains(msg, "refusing to write through symlink") {
		return http.StatusConflict, "SYMLINK_CONSENT_REQUIRED"
	}
	return http.StatusInternalServerError, "ADOPT_FAILED"
}

func installAdoptSymlinkConsent(targets []adoptSymlinkTarget) func() {
	adoptSymlinkConsentMu.Lock()
	prev := api.InteractiveSymlinkConsent
	api.InteractiveSymlinkConsent = func(_ string, originalPath string, pinnedPath string) bool {
		cleanOriginal := filepath.Clean(originalPath)
		cleanPinned := filepath.Clean(pinnedPath)
		for _, target := range targets {
			if cleanOriginal == target.originalPath && cleanPinned == target.pinnedPath {
				return true
			}
		}
		return false
	}
	return func() {
		api.InteractiveSymlinkConsent = prev
		adoptSymlinkConsentMu.Unlock()
	}
}

func formatAdoptSymlinkTargets(targets []adoptSymlinkTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, fmt.Sprintf("%s -> %s", target.Client, target.ResolvedPath))
	}
	return strings.Join(parts, ", ")
}
