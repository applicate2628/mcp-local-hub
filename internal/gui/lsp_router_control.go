package gui

import (
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type lspRouterControlAPI interface {
	DisableLSPRouterClient(string, api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error)
	EnableLSPRouterClient(string, api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error)
	LSPRouterClientStatuses(api.LSPClientRouterOpts) ([]api.LSPRouterClientStatus, error)
}

var lspRouterControlAPIFactory = func() lspRouterControlAPI { return api.NewAPI() }

type lspRouterControlRequest struct {
	Client string `json:"client"`
}

type lspRouterControlResponse struct {
	Client  string                     `json:"client"`
	Enabled bool                       `json:"enabled"`
	Report  *api.LSPClientRouterReport `json:"report"`
}

type lspRouterStatusResponse struct {
	Clients []lspRouterClientStatusResponse `json:"clients"`
}

type lspRouterClientStatusResponse struct {
	Client          string   `json:"client"`
	ConfigPath      string   `json:"config_path"`
	Disabled        bool     `json:"disabled"`
	ExistingEntries []string `json:"existing_entries,omitempty"`
	MissingEntries  []string `json:"missing_entries,omitempty"`
}

func registerLSPRouterControlRoutes(s *Server) {
	s.mux.HandleFunc("/api/lsp-router/status", s.requireSameOrigin(s.lspRouterStatusHandler))
	s.mux.HandleFunc("/api/lsp-router/disable", s.requireSameOrigin(s.lspRouterDisableHandler))
	s.mux.HandleFunc("/api/lsp-router/enable", s.requireSameOrigin(s.lspRouterEnableHandler))
}

func (s *Server) lspRouterStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a := lspRouterControlAPIFactory()
	statuses, err := a.LSPRouterClientStatuses(api.LSPClientRouterOpts{GUIPort: s.Port()})
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "LSP_ROUTER_STATUS_FAILED", "/api/lsp-router/status")
		return
	}
	clients := make([]lspRouterClientStatusResponse, 0, len(statuses))
	for _, status := range statuses {
		clients = append(clients, lspRouterClientStatusResponse{
			Client:          status.Client,
			ConfigPath:      status.ConfigPath,
			Disabled:        status.Disabled,
			ExistingEntries: append([]string(nil), status.ExistingEntries...),
			MissingEntries:  append([]string(nil), status.MissingEntries...),
		})
	}
	writeJSON(w, http.StatusOK, lspRouterStatusResponse{Clients: clients})
}

func (s *Server) lspRouterDisableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientName, ok := decodeLSPRouterControlClient(w, r)
	if !ok {
		return
	}
	if _, err := lspRouterControlClientAdapter(clientName); err != nil {
		writeAPIError(w, err, http.StatusBadRequest, "LSP_ROUTER_CLIENT_INVALID")
		return
	}
	a := lspRouterControlAPIFactory()
	report, err := a.DisableLSPRouterClient(clientName, api.LSPClientRouterOpts{GUIPort: s.Port()})
	writeLSPRouterControlResponse(w, s, "lsp-router-disable", clientName, false, report, err)
}

func (s *Server) lspRouterEnableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientName, ok := decodeLSPRouterControlClient(w, r)
	if !ok {
		return
	}
	if _, err := lspRouterControlClientAdapter(clientName); err != nil {
		writeAPIError(w, err, http.StatusBadRequest, "LSP_ROUTER_CLIENT_INVALID")
		return
	}
	a := lspRouterControlAPIFactory()
	report, err := a.EnableLSPRouterClient(clientName, api.LSPClientRouterOpts{GUIPort: s.Port()})
	writeLSPRouterControlResponse(w, s, "lsp-router-enable", clientName, true, report, err)
}

func decodeLSPRouterControlClient(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body lspRouterControlRequest
	if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "LSP_ROUTER_CONTROL_INVALID_JSON")
		return "", false
	}
	clientName := strings.TrimSpace(body.Client)
	if clientName == "" {
		writeAPIError(w, fmt.Errorf("client is required"), http.StatusBadRequest, "LSP_ROUTER_CLIENT_REQUIRED")
		return "", false
	}
	return clientName, true
}

func lspRouterControlClientAdapter(clientName string) (clients.Client, error) {
	adapter, ok := clients.AllClients()[clientName]
	if !ok || adapter == nil {
		return nil, fmt.Errorf("unknown client %q (expected %s)", clientName, strings.Join(clients.SupportedClientNames(), " | "))
	}
	return adapter, nil
}

func writeLSPRouterControlResponse(w http.ResponseWriter, s *Server, eventType, clientName string, enabled bool, report *api.LSPClientRouterReport, err error) {
	if report == nil {
		report = &api.LSPClientRouterReport{}
	}
	if err != nil && len(report.Failed) == 0 {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "LSP_ROUTER_CONTROL_FAILED", "/api/lsp-router control")
		return
	}
	status := http.StatusOK
	if len(report.Failed) > 0 {
		status = http.StatusMultiStatus
	}
	s.events.PublishOperatorAction(eventType, api.CurrentOSUser(), map[string]any{
		"client":         clientName,
		"enabled":        enabled,
		"applied_count":  len(report.Applied),
		"removed_count":  len(report.Removed),
		"restored_count": len(report.Restored),
		"skipped_count":  len(report.Skipped),
		"failed_count":   len(report.Failed),
	})
	writeJSON(w, status, lspRouterControlResponse{
		Client:  clientName,
		Enabled: enabled,
		Report:  report,
	})
}
