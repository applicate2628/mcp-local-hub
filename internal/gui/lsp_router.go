package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/lsp_routing"
	"mcp-local-hub/internal/config"
)

type lspWorkspaceResolver interface {
	ResolveByPath(path, language string) (*lsp_routing.ResolveResult, error)
	HasProjectMarker(root, language string) bool
	RegisteredWorkspace(workspaceKey, language string) (*api.WorkspaceEntry, bool)
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
	// TrustedRootCheckFn is the authorization gate for FIRST-TOUCH
	// auto-register. Given a resolved workspace root, it reports whether
	// that root is equal to, or a true subdirectory of, an operator-
	// trusted root (an operator-configured allowed root OR a root blessed
	// by a prior EXPLICIT register). workspaceFromResolvedLSPPath calls
	// this BEFORE AutoRegisterFn and refuses (exactly as PR #269 did)
	// when it returns false. An error fails CLOSED (refuse). Production
	// wires this to api.LSPWorkspaceRootTrusted, which reads the live
	// <state-dir>/lsp-trusted-roots.json store. When nil (legacy deps
	// without the gate wired), the router fails CLOSED on first touch:
	// an unset gate must never silently authorize untrusted paths.
	TrustedRootCheckFn func(workspaceRoot string) (bool, error)
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
		// Trusted-root gate: reads the live on-disk lsp-trusted-roots.json
		// store on each first-touch decision. NOTE this is the READ path
		// only — it never blesses. Blessing happens exclusively at the
		// explicit register call sites (internal/gui/lsp_register.go and
		// the `mcphub register` CLI path).
		TrustedRootCheckFn: api.LSPWorkspaceRootTrusted,
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
	case "resources/list":
		s.handleLSPResourcesList(w, &tb, backendKind)
		return
	case "prompts/list":
		s.handleLSPPromptsList(w, &tb, backendKind)
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

func (s *Server) handleLSPResourcesList(w http.ResponseWriter, tb *toolBody, backendKind string) {
	s.handleLSPEmptyList(w, tb, backendKind, api.SyntheticResourcesListResponse)
}

func (s *Server) handleLSPPromptsList(w http.ResponseWriter, tb *toolBody, backendKind string) {
	s.handleLSPEmptyList(w, tb, backendKind, api.SyntheticPromptsListResponse)
}

func (s *Server) handleLSPEmptyList(
	w http.ResponseWriter,
	tb *toolBody,
	backendKind string,
	build func(json.RawMessage, string) ([]byte, error),
) {
	if isJSONRPCNotificationID(tb.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !isValidJSONRPCRequestID(tb.ID) {
		writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
			"invalid request: id must be a non-null string or number")
		return
	}
	resp, err := build(tb.ID, backendKind)
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
	pathArgs, hasPath := lsp_routing.ExtractPathArgs(toolArguments)
	if !hasPath {
		ws, ok := lspPathlessWorkspace(w, r, deps, tb, language, sessionID)
		return ws, false, ok
	}

	resolved, ok := s.resolveLSPToolPathArg(w, deps, tb, language, pathArgs[0], sessionID)
	if !ok {
		return nil, false, false
	}
	for _, pathArg := range pathArgs[1:] {
		next, ok := s.resolveLSPToolPathArg(w, deps, tb, language, pathArg, sessionID)
		if !ok {
			return nil, false, false
		}
		if !sameLSPResolvedWorkspace(resolved, next) {
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
				"files span multiple LSP workspaces ("+lspResolvedWorkspaceLabel(resolved)+", "+lspResolvedWorkspaceLabel(next)+"); split the call per workspace")
			return nil, false, false
		}
	}
	entry, ok := s.workspaceFromResolvedLSPPath(w, r, deps, tb, resolved, language, pathArgs[0])
	return entry, sessionID != "", ok
}

func (s *Server) resolveLSPToolPathArg(
	w http.ResponseWriter,
	deps *lspRouterDeps,
	tb *toolBody,
	language string,
	pathArg string,
	sessionID string,
) (*lsp_routing.ResolveResult, bool) {
	resolved, err := deps.Resolver.ResolveByPath(pathArg, language)
	if err != nil && errors.Is(err, lsp_routing.ErrInvalidPath) && isRelativeLSPPathArg(pathArg) {
		if deps.Sessions != nil && sessionID != "" {
			candidates := deps.Sessions.Candidates(sessionID)
			switch len(candidates) {
			case 1:
				if joined, ok := relativeLSPPathUnderWorkspace(candidates[0].WorkspacePath, pathArg); ok {
					resolved, err = deps.Resolver.ResolveByPath(joined, language)
				}
			case 0:
			default:
				writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
					"ambiguous LSP workspace for relative path "+pathArg+"; candidates: "+lspCandidateList(candidates))
				return nil, false
			}
		}
	}
	if err != nil {
		if errors.Is(err, lsp_routing.ErrWorkspaceNotFound) || errors.Is(err, lsp_routing.ErrInvalidPath) {
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "no LSP workspace for path "+pathArg)
			return nil, false
		}
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"resolve LSP workspace: "+err.Error(), nil)
		return nil, false
	}
	if resolved == nil {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams, "no LSP workspace for path "+pathArg)
		return nil, false
	}
	return resolved, true
}

func (s *Server) workspaceFromResolvedLSPPath(
	w http.ResponseWriter,
	r *http.Request,
	deps *lspRouterDeps,
	tb *toolBody,
	resolved *lsp_routing.ResolveResult,
	language string,
	pathArg string,
) (*api.WorkspaceEntry, bool) {
	if resolved.Registered {
		if deps.AutoRegisterFn != nil {
			entry, ok := s.ensureResolvedLSPWorkspace(w, r, deps, tb, resolved, language)
			return entry, ok
		}
		if resolved.Entry == nil {
			writeJSONRPCError(w, tb.ID, jsonrpcInternalError, "registered LSP workspace has no registry entry")
			return nil, false
		}
		return resolved.Entry, true
	}
	// First-touch auto-register. The ProjectMarker is NOT an
	// authorization boundary — it is only a discovery hint (per PR #269's
	// reasoning, an attacker-named path commonly carries a marker). The
	// trusted-root gate below is the authorization boundary: auto-register
	// proceeds ONLY when the resolved workspace root is equal to, or a
	// true subdirectory of, an operator-trusted root (an operator-
	// configured allowed root OR a root blessed by a prior EXPLICIT
	// register). Untrusted MCP tool arguments must never authorize a new
	// local LSP workspace. The store lives at
	// <state-dir>/lsp-trusted-roots.json; see
	// internal/api/lsp_trusted_roots.go.
	if !s.lspWorkspaceRootIsTrusted(deps, resolved) {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
			"LSP workspace for "+pathArg+" is not registered; run mcphub register for this workspace before using the LSP router")
		return nil, false
	}
	if deps.AutoRegisterFn == nil {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"LSP auto-register is not configured", nil)
		return nil, false
	}
	entry, ok := s.ensureResolvedLSPWorkspace(w, r, deps, tb, resolved, language)
	return entry, ok
}

// lspWorkspaceRootIsTrusted is the router-side adapter over the
// trusted-root authorization gate. It resolves the workspace root from
// the ResolveResult (the resolver supplies resolved.WorkspaceRoot in
// canonical form for an unregistered first-touch; fall back to
// resolved.Entry.WorkspacePath defensively), then consults
// deps.TrustedRootCheckFn.
//
// Fail-CLOSED on every uncertainty: a nil gate (legacy deps without the
// gate wired), an empty workspace root, or a gate error (corrupt store,
// insecure-parent rejection) all return false so the caller emits the
// "not registered" refusal. An unset or erroring gate must never
// silently authorize an untrusted path.
func (s *Server) lspWorkspaceRootIsTrusted(deps *lspRouterDeps, resolved *lsp_routing.ResolveResult) bool {
	if deps == nil || deps.TrustedRootCheckFn == nil {
		return false
	}
	workspaceRoot := resolved.WorkspaceRoot
	if workspaceRoot == "" && resolved.Entry != nil {
		workspaceRoot = resolved.Entry.WorkspacePath
	}
	if workspaceRoot == "" {
		return false
	}
	trusted, err := deps.TrustedRootCheckFn(workspaceRoot)
	if err != nil {
		return false
	}
	return trusted
}

func isRelativeLSPPathArg(pathArg string) bool {
	return pathArg != "" && filepath.VolumeName(pathArg) == "" && !filepath.IsAbs(pathArg)
}

func relativeLSPPathUnderWorkspace(workspaceRoot, pathArg string) (string, bool) {
	if workspaceRoot == "" || !isRelativeLSPPathArg(pathArg) {
		return "", false
	}
	root := filepath.Clean(workspaceRoot)
	joined := filepath.Clean(filepath.Join(root, pathArg))
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return joined, true
}

func sameLSPResolvedWorkspace(a, b *lsp_routing.ResolveResult) bool {
	aKey, aRoot := lspResolvedWorkspaceIdentity(a)
	bKey, bRoot := lspResolvedWorkspaceIdentity(b)
	if aKey != "" && bKey != "" {
		return aKey == bKey
	}
	return aRoot != "" && aRoot == bRoot
}

func lspResolvedWorkspaceIdentity(res *lsp_routing.ResolveResult) (workspaceKey, workspaceRoot string) {
	if res == nil {
		return "", ""
	}
	workspaceKey = res.WorkspaceKey
	workspaceRoot = res.WorkspaceRoot
	if res.Entry != nil {
		if workspaceKey == "" {
			workspaceKey = res.Entry.WorkspaceKey
		}
		if workspaceRoot == "" {
			workspaceRoot = res.Entry.WorkspacePath
		}
	}
	return workspaceKey, workspaceRoot
}

func lspResolvedWorkspaceLabel(res *lsp_routing.ResolveResult) string {
	workspaceKey, workspaceRoot := lspResolvedWorkspaceIdentity(res)
	if workspaceRoot != "" && workspaceKey != "" {
		return fmt.Sprintf("%s (%s)", workspaceRoot, workspaceKey)
	}
	if workspaceRoot != "" {
		return workspaceRoot
	}
	if workspaceKey != "" {
		return workspaceKey
	}
	return "unknown"
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
		markerRoot := ws.WorkspacePath
		if markerRoot == "" {
			markerRoot = ws.WorkspaceKey
		}
		if registered, ok := deps.Resolver.RegisteredWorkspace(ws.WorkspaceKey, language); ok && registered != nil {
			ws = *registered
		} else if !deps.Resolver.HasProjectMarker(ws.WorkspacePath, language) {
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidParams,
				"no language project marker for "+language+" under "+markerRoot+"; refusing .git-only LSP auto-register")
			return nil, false
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
