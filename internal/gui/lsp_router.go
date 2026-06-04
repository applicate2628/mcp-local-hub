package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
	"mcp-local-hub/internal/config"
)

type lspWorkspaceResolver interface {
	ResolveByPath(path, language string) (*lsp_routing.ResolveResult, error)
}

type lspSessionRouter interface {
	EnsureSession(sessionID string)
	TouchWorkspace(sessionID string, ws *api.WorkspaceEntry)
	Candidates(sessionID string) []api.WorkspaceEntry
	UnbindSession(sessionID string)
}

type lspRouterDeps struct {
	Resolver               lspWorkspaceResolver
	Sessions               lspSessionRouter
	HTTPClient             *http.Client
	UpstreamURLFn          func(ws *api.WorkspaceEntry) string
	UpstreamTimeout        time.Duration
	BackendKindForLanguage func(language string) (string, bool)
	AutoRegisterFn         func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error)
}

var lspRouterTestSeam func() *lspRouterDeps

func (s *Server) SetLSPRouterProduction(resolver *lsp_routing.WorkspaceResolver, sessions *lsp_routing.SessionRouter, languages []config.LanguageSpec) {
	if s == nil || resolver == nil || sessions == nil {
		return
	}
	backendByLanguage := map[string]string{}
	for _, spec := range languages {
		if spec.Name != "" && spec.Backend != "" {
			backendByLanguage[spec.Name] = spec.Backend
		}
	}
	s.SetLSPRouterDeps(&lspRouterDeps{
		Resolver: resolver,
		Sessions: sessions,
		BackendKindForLanguage: func(language string) (string, bool) {
			kind, ok := backendByLanguage[language]
			return kind, ok
		},
		AutoRegisterFn: func(ctx context.Context, wsKey, workspacePath, language string) (*api.WorkspaceEntry, error) {
			entry, err := api.NewAPI().EnsureLSPRegistered(ctx, wsKey, workspacePath, language)
			if err != nil {
				return nil, err
			}
			return &entry, nil
		},
	})
}

func (s *Server) SetLSPRouterDeps(deps *lspRouterDeps) {
	s.lspRouterDeps.Store(deps)
}

func (s *Server) lspRouterDepsProd() *lspRouterDeps {
	if lspRouterTestSeam != nil {
		return lspRouterTestSeam()
	}
	return s.lspRouterDeps.Load()
}

func registerLSPRouterRoutes(s *Server) {
	s.mux.HandleFunc("/lsp/", s.requireSameOrigin(s.lspRouterHandler))
}

func (s *Server) lspRouterHandler(w http.ResponseWriter, r *http.Request) {
	language, ok := parseLSPRouterLanguage(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		sessionID := r.Header.Get("Mcp-Session-Id")
		if deps := s.lspRouterDepsProd(); deps != nil && deps.Sessions != nil {
			deps.Sessions.UnbindSession(sessionID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deps := s.lspRouterDepsProd()
	if deps == nil || deps.Resolver == nil {
		writeJSONRPCError(w, nil, jsonrpcInternalError, "lsp router is not configured")
		return
	}
	backendKind, ok := lspBackendKind(deps, language)
	if !ok {
		writeJSONRPCError(w, nil, jsonrpcInvalidParams, "unknown LSP language "+language)
		return
	}
	upstreamURLFn := deps.UpstreamURLFn
	if upstreamURLFn == nil {
		upstreamURLFn = defaultUpstreamURL
	}
	upstreamTimeout := deps.UpstreamTimeout
	if upstreamTimeout <= 0 {
		upstreamTimeout = serenaUpstreamTimeout
	}
	httpClient := serenaHTTPClient(deps.HTTPClient, upstreamTimeout)

	const maxBodyBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "request body exceeds 4 MiB", http.StatusBadRequest)
		return
	}

	var tb toolBody
	if err := json.Unmarshal(body, &tb); err != nil {
		http.Error(w, "malformed JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if tb.JSONRPC != "2.0" {
		echoID := tb.ID
		if !isValidJSONRPCRequestID(echoID) {
			echoID = nil
		}
		writeJSONRPCErrorStatus(w, echoID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"invalid request: jsonrpc must be \"2.0\"", nil)
		return
	}
	if tb.Method == "" {
		http.Error(w, "missing required field: method", http.StatusBadRequest)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")
	switch tb.Method {
	case "initialize":
		s.handleLSPInitialize(w, &tb, backendKind)
		return
	case "tools/list":
		s.handleLSPToolsList(w, &tb, backendKind)
		return
	case "ping":
		if isJSONRPCNotificationID(tb.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if !isValidJSONRPCRequestID(tb.ID) {
			writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
				"invalid request: id must be a non-null string or number")
			return
		}
		writeJSONRPCResult(w, tb.ID, map[string]any{}, nil)
		return
	case "notifications/initialized":
		if !isJSONRPCNotificationID(tb.ID) {
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest,
				"invalid request: notifications/* must not include id")
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	default:
		if strings.HasPrefix(tb.Method, "notifications/") {
			s.handleLSPNotification(w, r, deps, &tb, body, httpClient, upstreamURLFn, sessionID, upstreamTimeout)
			return
		}
	}

	if tb.Method != "tools/call" {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest, "unsupported method "+tb.Method)
		return
	}
	if isJSONRPCNotificationID(tb.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !isValidJSONRPCRequestID(tb.ID) {
		writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
			"invalid request: id must be a non-null string or number")
		return
	}

	toolName, toolArguments := lsp_routing.ToolCallParams(tb.Params)
	if toolName == "" {
		writeRequiredFieldError(w, "params.name")
		return
	}
	ws, bindAfter, resolved := s.resolveLSPToolWorkspace(w, r, deps, &tb, language, toolArguments, sessionID)
	if !resolved {
		return
	}
	if s.forwardLSPToWorkspace(w, r, body, httpClient, upstreamURLFn, ws, upstreamTimeout) && bindAfter && deps.Sessions != nil {
		deps.Sessions.TouchWorkspace(sessionID, ws)
	}
}

func parseLSPRouterLanguage(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/lsp/")
	if rest == path || rest == "" {
		return "", false
	}
	language, tail, ok := strings.Cut(rest, "/")
	if !ok || language == "" || tail != "mcp" {
		return "", false
	}
	return language, true
}

func lspBackendKind(deps *lspRouterDeps, language string) (string, bool) {
	if deps == nil || deps.BackendKindForLanguage == nil {
		return "", false
	}
	return deps.BackendKindForLanguage(language)
}

func (s *Server) handleLSPInitialize(w http.ResponseWriter, tb *toolBody, backendKind string) {
	if isJSONRPCNotificationID(tb.ID) {
		writeJSONRPCErrorStatus(w, json.RawMessage("null"), http.StatusBadRequest, jsonrpcInvalidRequest,
			"invalid request: initialize requires a non-null id", nil)
		return
	}
	if !isValidJSONRPCRequestID(tb.ID) {
		writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
			"invalid request: id must be a non-null string or number")
		return
	}
	resp, err := api.SyntheticInitializeResponse(tb.ID, backendKind)
	if err != nil {
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError, err.Error())
		return
	}
	sessionID := newMcpSessionID()
	if deps := s.lspRouterDepsProd(); deps != nil && deps.Sessions != nil {
		deps.Sessions.EnsureSession(sessionID)
	}
	w.Header().Set("Mcp-Session-Id", sessionID)
	writeLSPRawJSON(w, resp)
}

func (s *Server) handleLSPToolsList(w http.ResponseWriter, tb *toolBody, backendKind string) {
	if isJSONRPCNotificationID(tb.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !isValidJSONRPCRequestID(tb.ID) {
		writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
			"invalid request: id must be a non-null string or number")
		return
	}
	resp, err := api.SyntheticToolsListResponse(tb.ID, backendKind)
	if err != nil {
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError, err.Error())
		return
	}
	writeLSPRawJSON(w, resp)
}

func writeLSPRawJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) resolveLSPToolWorkspace(
	w http.ResponseWriter,
	r *http.Request,
	deps *lspRouterDeps,
	tb *toolBody,
	language string,
	toolArguments json.RawMessage,
	sessionID string,
) (*api.WorkspaceEntry, bool, bool) {
	pathArg, hasPath := lsp_routing.ExtractPathArg(toolArguments)
	if !hasPath {
		ws, ok := lspPathlessWorkspace(w, r, deps, tb, language, sessionID)
		return ws, false, ok
	}

	resolved, err := deps.Resolver.ResolveByPath(pathArg, language)
	if err != nil {
		if errors.Is(err, lsp_routing.ErrWorkspaceNotFound) || errors.Is(err, lsp_routing.ErrInvalidPath) {
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "no LSP workspace for path "+pathArg)
			return nil, false, false
		}
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"resolve LSP workspace: "+err.Error(), nil)
		return nil, false, false
	}
	if resolved == nil {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "no LSP workspace for path "+pathArg)
		return nil, false, false
	}
	if resolved.Registered {
		if deps.AutoRegisterFn != nil {
			entry, ok := s.ensureResolvedLSPWorkspace(w, r, deps, tb, resolved, language)
			return entry, sessionID != "", ok
		}
		if resolved.Entry == nil {
			writeJSONRPCError(w, tb.ID, jsonrpcInternalError, "registered LSP workspace has no registry entry")
			return nil, false, false
		}
		return resolved.Entry, sessionID != "", true
	}
	if !resolved.ProjectMarker {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
			"no language project marker for "+language+" under "+pathArg+"; refusing .git-only LSP auto-register")
		return nil, false, false
	}
	if deps.AutoRegisterFn == nil {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"LSP auto-register is not configured", nil)
		return nil, false, false
	}
	entry, ok := s.ensureResolvedLSPWorkspace(w, r, deps, tb, resolved, language)
	return entry, sessionID != "", ok
}

func (s *Server) ensureResolvedLSPWorkspace(
	w http.ResponseWriter,
	r *http.Request,
	deps *lspRouterDeps,
	tb *toolBody,
	resolved *lsp_routing.ResolveResult,
	language string,
) (*api.WorkspaceEntry, bool) {
	wsKey := resolved.WorkspaceKey
	workspaceRoot := resolved.WorkspaceRoot
	if resolved.Entry != nil {
		if wsKey == "" {
			wsKey = resolved.Entry.WorkspaceKey
		}
		if workspaceRoot == "" {
			workspaceRoot = resolved.Entry.WorkspacePath
		}
	}
	if wsKey == "" || workspaceRoot == "" {
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError, "resolved LSP workspace has no workspace identity")
		return nil, false
	}
	regCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	entry, err := deps.AutoRegisterFn(regCtx, wsKey, workspaceRoot, language)
	if err != nil {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"LSP auto-register failed: "+err.Error(), nil)
		return nil, false
	}
	if entry == nil {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"LSP auto-register returned no entry", nil)
		return nil, false
	}
	return entry, true
}

func lspPathlessWorkspace(w http.ResponseWriter, r *http.Request, deps *lspRouterDeps, tb *toolBody, language, sessionID string) (*api.WorkspaceEntry, bool) {
	if sessionID == "" || deps == nil || deps.Sessions == nil {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "make a file-scoped call first")
		return nil, false
	}
	candidates := deps.Sessions.Candidates(sessionID)
	switch len(candidates) {
	case 0:
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "make a file-scoped call first")
		return nil, false
	case 1:
		ws := candidates[0]
		if deps.AutoRegisterFn == nil {
			return &ws, true
		}
		regCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
		defer cancel()
		entry, err := deps.AutoRegisterFn(regCtx, ws.WorkspaceKey, ws.WorkspacePath, language)
		if err != nil {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
				"LSP re-ensure failed for pathless call: "+err.Error(), nil)
			return nil, false
		}
		if entry == nil {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
				"LSP re-ensure returned no entry", nil)
			return nil, false
		}
		return entry, true
	default:
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
			"ambiguous LSP workspace for pathless call; candidates: "+lspCandidateList(candidates))
		return nil, false
	}
}

func lspCandidateList(candidates []api.WorkspaceEntry) string {
	parts := make([]string, 0, len(candidates))
	for _, ws := range candidates {
		label := ws.WorkspacePath
		if label == "" {
			label = ws.WorkspaceKey
		}
		if ws.WorkspaceKey != "" && ws.WorkspacePath != "" {
			label = fmt.Sprintf("%s (%s)", ws.WorkspacePath, ws.WorkspaceKey)
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func (s *Server) handleLSPNotification(
	w http.ResponseWriter,
	r *http.Request,
	deps *lspRouterDeps,
	tb *toolBody,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	sessionID string,
	upstreamTimeout time.Duration,
) {
	if !isJSONRPCNotificationID(tb.ID) {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest,
			"invalid request: notifications/* must not include id")
		return
	}
	if sessionID == "" || deps == nil || deps.Sessions == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	candidates := deps.Sessions.Candidates(sessionID)
	if len(candidates) != 1 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ws := candidates[0]
	if s.forwardLSPToWorkspace(w, r, body, httpClient, upstreamURLFn, &ws, upstreamTimeout) {
		return
	}
}

func (s *Server) forwardLSPToWorkspace(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	ws *api.WorkspaceEntry,
	upstreamTimeout time.Duration,
) bool {
	upstreamURL := upstreamURLFn(ws)
	if upstreamURL == "" {
		writeJSONRPCError(w, nil, jsonrpcInternalError, "LSP upstream URL is empty")
		return false
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeJSONRPCError(w, nil, jsonrpcInternalError, "build LSP upstream request: "+err.Error())
		return false
	}
	for _, h := range []string{"Content-Type", "Accept", "MCP-Protocol-Version"} {
		if v := r.Header.Get(h); v != "" {
			upstreamReq.Header.Set(h, v)
		}
	}
	if upstreamReq.Header.Get("Content-Type") == "" {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}
	upstreamReq.Header.Del("Mcp-Session-Id")

	upstreamResp, err := httpClient.Do(upstreamReq)
	if err != nil {
		if isTimeoutErr(err) {
			http.Error(w, fmt.Sprintf("upstream LSP proxy at port %d did not respond within %ds", ws.Port, int(upstreamTimeout/time.Second)), http.StatusGatewayTimeout)
			return false
		}
		http.Error(w, fmt.Sprintf("upstream LSP proxy at port %d unreachable: %s", ws.Port, err.Error()), http.StatusBadGateway)
		return false
	}
	defer upstreamResp.Body.Close()

	copyHeaders(w.Header(), upstreamResp.Header)
	w.Header().Del("Mcp-Session-Id")
	contentType := upstreamResp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	w.WriteHeader(upstreamResp.StatusCode)
	if isSSE {
		streamSSE(w, upstreamResp.Body)
		return true
	}
	_, _ = io.Copy(w, upstreamResp.Body)
	return true
}
