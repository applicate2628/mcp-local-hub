package gui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"mcp-local-hub/internal/api"
)

var ensureLSPRegisteredForGUI = func(ctx context.Context, workspaceKey, workspacePath, language string) (api.WorkspaceEntry, error) {
	return api.NewAPI().EnsureLSPRegistered(ctx, workspaceKey, workspacePath, language)
}

type lspRegisterLanguageResult struct {
	Language   string `json:"language"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	diagnostic *api.RegistrationDiagnostic
}

type lspRegisterReport struct {
	Workspace    string
	WorkspaceKey string
	Entries      []api.WorkspaceEntry
	Diagnostics  []api.RegistrationDiagnostic
	Results      []lspRegisterLanguageResult
}

type lspRegisterResponse struct {
	Workspace    string                      `json:"workspace"`
	WorkspaceKey string                      `json:"workspace_key"`
	Entries      []workspaceEntryDTO         `json:"entries"`
	Warnings     []string                    `json:"warnings,omitempty"`
	Results      []lspRegisterLanguageResult `json:"results,omitempty"`
	Error        string                      `json:"error,omitempty"`
	Code         string                      `json:"code,omitempty"`
}

type lspRegisterRequest struct {
	WorkspacePath string   `json:"workspace_path"`
	Language      string   `json:"language,omitempty"`
	Languages     []string `json:"languages,omitempty"`
}

func (realLSPRegistrar) RegisterLSP(workspacePath string, languages []string) (*lspRegisterReport, error) {
	report := &lspRegisterReport{Workspace: workspacePath}
	blessedRoot := ""
	for _, language := range languages {
		entry, err := ensureLSPRegisteredForGUI(context.Background(), "", workspacePath, language)
		if err != nil {
			diagnostic := api.ClassifyLSPEnsureError(err, language, "lsp-register")
			report.Results = append(report.Results, lspRegisterLanguageResult{
				Language:   language,
				Status:     "error",
				Error:      registrationDiagnosticPublicText(diagnostic),
				diagnostic: &diagnostic,
			})
			continue
		}
		report.Entries = append(report.Entries, entry)
		report.Results = append(report.Results, lspRegisterLanguageResult{
			Language: language,
			Status:   "ok",
		})
		if report.Workspace == "" || report.Workspace == workspacePath {
			report.Workspace = entry.WorkspacePath
		}
		if report.WorkspaceKey == "" {
			report.WorkspaceKey = entry.WorkspaceKey
		}
		// The GUI "Enable" / lsp-register handler is an EXPLICIT operator
		// action, so a successful registration blesses the workspace's
		// canonical root for the router's first-touch auto-register gate
		// (same seed semantics as the `mcphub register` CLI path). The
		// router's own auto-register seam does NOT reach here, so an
		// untrusted tool-call path can never bless itself. entry.WorkspacePath
		// is the CanonicalWorkspacePath form EnsureLSPRegistered persisted;
		// it is identical across the languages of one register call, so
		// bless once.
		if blessedRoot == "" && entry.WorkspacePath != "" {
			blessedRoot = entry.WorkspacePath
		}
	}
	if blessedRoot != "" {
		if err := blessLSPTrustedRootForGUI(blessedRoot); err != nil {
			// Best-effort: the register succeeded; surface the bless
			// failure as a warning so a later sibling-workspace
			// auto-register failure is diagnosable, but never fail the
			// register over it.
			report.Diagnostics = append(report.Diagnostics, api.NewRegistrationDiagnostic(
				api.RegistrationCodeTrustedRootRecordFailed, "trusted-root", "lsp-register", err,
			))
		}
	}
	return report, nil
}

// blessLSPTrustedRootForGUI is the explicit-register bless seam for the
// GUI lsp-register handler. A package-level var so tests can assert it
// fires at the explicit register site — and assert the ROUTER path never
// reaches it. Production delegates to api.BlessDefaultTrustedRoot.
var blessLSPTrustedRootForGUI = func(workspaceRoot string) error {
	return api.BlessDefaultTrustedRoot(workspaceRoot)
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
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
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
		diagnostic := api.ClassifyRegistrationError(err, "lsp-register", "handler")
		writeRegistrationDiagnosticError(w, err, diagnostic, http.StatusInternalServerError, "LSP_REGISTER_FAILED", "/api/lsp/register")
		return
	}
	if report == nil {
		report = &lspRegisterReport{}
	}
	resp := lspRegisterResponseFromReport(report)
	status := http.StatusOK
	if lspRegisterResponseHasFailures(resp) {
		if len(resp.Entries) > 0 {
			status = http.StatusMultiStatus
		} else {
			status = http.StatusInternalServerError
			resp.Code = "LSP_REGISTER_FAILED"
			resp.Error = firstLSPRegisterError(resp.Results)
			if resp.Error == "" {
				resp.Error = "LSP register failed"
			}
		}
	}
	writeJSON(w, status, resp)
}

func lspRegisterResponseFromReport(report *lspRegisterReport) lspRegisterResponse {
	if report == nil {
		report = &lspRegisterReport{}
	}
	entries := make([]workspaceEntryDTO, 0, len(report.Entries))
	for _, entry := range report.Entries {
		entries = append(entries, workspaceEntryDTOFromAPI(entry))
	}
	results := append([]lspRegisterLanguageResult(nil), report.Results...)
	for i := range results {
		if results[i].diagnostic != nil {
			results[i].Error = registrationDiagnosticPublicText(*results[i].diagnostic)
		}
	}
	warnings := projectRegistrationWarnings(report.Diagnostics)
	return lspRegisterResponse{
		Workspace:    report.Workspace,
		WorkspaceKey: report.WorkspaceKey,
		Entries:      entries,
		Warnings:     warnings,
		Results:      results,
	}
}

func lspRegisterResponseHasFailures(resp lspRegisterResponse) bool {
	for _, result := range resp.Results {
		if result.Status == "error" {
			return true
		}
	}
	return false
}

func firstLSPRegisterError(results []lspRegisterLanguageResult) string {
	for _, result := range results {
		if result.Status == "error" && result.Error != "" {
			return result.Error
		}
	}
	return ""
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
