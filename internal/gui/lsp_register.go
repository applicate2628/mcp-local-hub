package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"mcp-local-hub/internal/api"
)

var ensureLSPRegisteredForGUI = func(ctx context.Context, workspaceKey, workspacePath, language string) (api.WorkspaceEntry, error) {
	return api.NewAPI().EnsureLSPRegistered(ctx, workspaceKey, workspacePath, language)
}

type lspRegisterRequest struct {
	WorkspacePath string   `json:"workspace_path"`
	Language      string   `json:"language,omitempty"`
	Languages     []string `json:"languages,omitempty"`
}

func (realLSPRegistrar) RegisterLSP(workspacePath string, languages []string) (*api.RegisterReport, error) {
	report := &api.RegisterReport{Workspace: workspacePath}
	for _, language := range languages {
		entry, err := ensureLSPRegisteredForGUI(context.Background(), "", workspacePath, language)
		if err != nil {
			return report, err
		}
		report.Entries = append(report.Entries, entry)
		if report.Workspace == "" || report.Workspace == workspacePath {
			report.Workspace = entry.WorkspacePath
		}
		if report.WorkspaceKey == "" {
			report.WorkspaceKey = entry.WorkspaceKey
		}
	}
	return report, nil
}

func registerLSPRegisterRoutes(s *Server) {
	s.mux.HandleFunc("/api/lsp/register", s.requireSameOrigin(s.lspRegisterHandler))
}

func (s *Server) lspRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req lspRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	workspacePath := strings.TrimSpace(req.WorkspacePath)
	if workspacePath == "" {
		writeAPIError(w, fmt.Errorf("workspace_path is required"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}
	languages := normalizeLSPRegisterLanguages(req)
	if len(languages) == 0 {
		writeAPIError(w, fmt.Errorf("language is required"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	report, err := s.lspRegistrar.RegisterLSP(workspacePath, languages)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_REGISTER_FAILED")
		return
	}
	if report == nil {
		report = &api.RegisterReport{}
	}
	writeJSON(w, http.StatusOK, report)
}

func normalizeLSPRegisterLanguages(req lspRegisterRequest) []string {
	raw := make([]string, 0, len(req.Languages)+1)
	if req.Language != "" {
		raw = append(raw, req.Language)
	}
	raw = append(raw, req.Languages...)
	out := make([]string, 0, len(raw))
	for _, lang := range raw {
		lang = strings.TrimSpace(lang)
		if lang == "" || slices.Contains(out, lang) {
			continue
		}
		out = append(out, lang)
	}
	return out
}
