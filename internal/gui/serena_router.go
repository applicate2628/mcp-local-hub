// internal/gui/serena_router.go
//
// POST /serena/mcp -- path-aware MCP request router for per-workspace
// serena daemons (plan v10 Phase C.2).
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

// ErrWorkspaceNotFound re-exports the A1 package sentinel so callers
// outside `internal/api/serena_routing` can `errors.Is(err, gui.ErrWorkspaceNotFound)`
// without importing the routing package directly. Both names refer to
// the SAME underlying error value — `errors.Is` works across the
// re-export boundary.
var ErrWorkspaceNotFound = serena_routing.ErrWorkspaceNotFound

// workspaceResolver is the narrow interface the handler uses to map
// an absolute or workspace-relative path to its owning workspace.
type workspaceResolver interface {
	ResolveByPath(path string) (*api.WorkspaceEntry, error)
}

// sessionRouter is the narrow interface for sticky session binding.
type sessionRouter interface {
	BindSession(sessionID string, ws *api.WorkspaceEntry)
	LookupSession(sessionID string) *api.WorkspaceEntry
	UnbindSession(sessionID string)
}

// serenaRouterDeps bundles the test seams so callers swap them
// atomically. Tests inject fakes via serenaRouterTestSeam.
type serenaRouterDeps struct {
	Resolver        workspaceResolver
	Sessions        sessionRouter
	HTTPClient      *http.Client
	UpstreamURLFn   func(ws *api.WorkspaceEntry) string
	AuditFn         func(level, event string, fields map[string]any) error
	UpstreamTimeout time.Duration
}

// serenaRouterTestSeam lets tests inject a fully-mocked deps bundle.
var serenaRouterTestSeam func() *serenaRouterDeps

// serenaUpstreamTimeout caps the per-forward connect + first-byte
// budget. Matches the 60s ceiling HTTPHost's httpClient uses for
// tool-call traffic. Tests override via UpstreamTimeout.
const serenaUpstreamTimeout = 60 * time.Second

func newSerenaTransport(upstreamTimeout time.Duration) *http.Transport {
	if upstreamTimeout <= 0 {
		upstreamTimeout = serenaUpstreamTimeout
	}
	dialTimeout := 5 * time.Second
	if upstreamTimeout < dialTimeout {
		dialTimeout = upstreamTimeout
	}
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: dialTimeout,
		}).DialContext,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: upstreamTimeout,
		DisableCompression:    true,
	}
}

// defaultSerenaClient is the production http.Client. Client.Timeout
// stays zero so a long-lived SSE stream is not killed mid-flight; the
// transport's dial timeout + ResponseHeaderTimeout enforce the
// connect + first-byte budget independently from body streaming.
var defaultSerenaClient = &http.Client{
	Timeout:   0,
	Transport: newSerenaTransport(serenaUpstreamTimeout),
}

func serenaHTTPClient(httpClient *http.Client, upstreamTimeout time.Duration) *http.Client {
	if httpClient != nil {
		return httpClient
	}
	if upstreamTimeout == serenaUpstreamTimeout {
		return defaultSerenaClient
	}
	return &http.Client{
		Timeout:   0,
		Transport: newSerenaTransport(upstreamTimeout),
	}
}

// serenaRouterDepsProd returns the production deps bundle. When the
// test seam is set, returns its output; otherwise reads the Server's
// atomic deps slot. A nil return means the routing layer is not yet
// wired -- the handler then emits 503 with the canonical body.
func (s *Server) serenaRouterDepsProd() *serenaRouterDeps {
	if serenaRouterTestSeam != nil {
		return serenaRouterTestSeam()
	}
	return s.serenaRouterDeps.Load()
}

// SetSerenaRouterProduction wires the production resolver + session
// router from A1's serena_routing package. Callable from internal/cli
// at GUI server construction time (cannot construct the unexported
// serenaRouterDeps from outside this package, so this exported helper
// fills the role).
func (s *Server) SetSerenaRouterProduction(resolver *serena_routing.WorkspaceResolver, sessions *serena_routing.SessionRouter) {
	if s == nil || resolver == nil || sessions == nil {
		return
	}
	s.SetSerenaRouterDeps(&serenaRouterDeps{
		Resolver: resolver,
		Sessions: sessions,
	})
}

// SetSerenaRouterDeps wires the production resolver + session router.
// CLI boot (cmd/mcphub) calls this after constructing Agent A1's
// adapters from the live api.Registry. Calling with nil clears the
// wiring (the route then emits 503).
func (s *Server) SetSerenaRouterDeps(deps *serenaRouterDeps) {
	s.serenaRouterDeps.Store(deps)
}

// NewInMemorySessionRouter returns a process-local sessionRouter for
// production callers who want sticky-session binding without depending
// on Agent A1's serena_routing package directly.
func NewInMemorySessionRouter() *InMemorySessionRouter {
	return newInMemorySessionRouter()
}

func registerSerenaRouterRoutes(s *Server) {
	s.mux.HandleFunc("/serena/mcp", s.requireSameOrigin(s.serenaRouterHandler))
}

// toolBody is the narrow shape we decode from the incoming MCP body.
// We re-encode the raw bytes verbatim when forwarding, so this struct
// only carries the fields we need to make routing decisions.
type toolBody struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// extractPathArg scans the tool body's arguments for any of the known
// serena path-arg field names. Returns ("", false) when none is
// present or all are empty strings.
//
// Field set (per plan section C.2 + the serena tool schema):
//   - relative_path : symbol/file ops
//   - file_path     : edit_file, read_file
//   - name_path     : insert_after_symbol, replace_symbol_body
//   - path          : list_dir, search_for_pattern
func extractPathArg(arguments json.RawMessage) (string, bool) {
	if len(arguments) == 0 {
		return "", false
	}
	var args map[string]json.RawMessage
	if uerr := json.Unmarshal(arguments, &args); uerr != nil {
		return "", false
	}
	for _, key := range []string{"relative_path", "file_path", "name_path", "path"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		var v string
		if uerr := json.Unmarshal(raw, &v); uerr != nil {
			continue
		}
		if v == "" {
			continue
		}
		return v, true
	}
	return "", false
}

// notFoundJSON is the canonical body returned on workspace-not-found.
type notFoundJSON struct {
	Error          string `json:"error"`
	PhaseEStatus   string `json:"phase_e_status"`
	HintCommand    string `json:"hint_command,omitempty"`
	ResolvedPath   string `json:"resolved_path,omitempty"`
	MissingSession bool   `json:"missing_session,omitempty"`
}

// writeWorkspaceNotFound emits the 503 with the canonical body.
func writeWorkspaceNotFound(w http.ResponseWriter, resolvedPath string, missingSession bool) {
	body := notFoundJSON{
		Error:        "register workspace first via mcphub workspace register <path>",
		PhaseEStatus: "deferred",
		HintCommand:  "mcphub workspace register <path>",
	}
	if resolvedPath != "" {
		body.ResolvedPath = resolvedPath
	}
	if missingSession {
		body.MissingSession = true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRequiredFieldError(w http.ResponseWriter, field string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `{"error": "missing required field: %s"}`, field)
}

func (s *Server) serenaRouterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deps := s.serenaRouterDepsProd()
	if deps == nil || deps.Resolver == nil {
		writeWorkspaceNotFound(w, "", false)
		return
	}
	auditFn := deps.AuditFn
	if auditFn == nil {
		auditFn = api.LogHubMcpEvent
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
	body, rerr := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if rerr != nil {
		http.Error(w, "read body: "+rerr.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "request body exceeds 4 MiB", http.StatusBadRequest)
		return
	}

	var tb toolBody
	if uerr := json.Unmarshal(body, &tb); uerr != nil {
		http.Error(w, "malformed JSON body: "+uerr.Error(), http.StatusBadRequest)
		return
	}
	if tb.Method == "" {
		http.Error(w, "missing required field: method", http.StatusBadRequest)
		return
	}
	if tb.Params.Name == "" {
		writeRequiredFieldError(w, "params.name")
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")

	pathArg, hasPath := extractPathArg(tb.Params.Arguments)

	var ws *api.WorkspaceEntry
	if hasPath {
		resolved, resolveErr := deps.Resolver.ResolveByPath(pathArg)
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrWorkspaceNotFound) {
				writeWorkspaceNotFound(w, pathArg, false)
				return
			}
			http.Error(w, "resolve workspace: "+resolveErr.Error(), http.StatusInternalServerError)
			return
		}
		if resolved == nil {
			writeWorkspaceNotFound(w, pathArg, false)
			return
		}
		ws = resolved
		if sessionID != "" && deps.Sessions != nil {
			deps.Sessions.BindSession(sessionID, ws)
		}
	} else {
		if sessionID == "" || deps.Sessions == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
		ws = deps.Sessions.LookupSession(sessionID)
		if ws == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
	}

	upstreamURL := upstreamURLFn(ws)
	if upstreamURL == "" {
		http.Error(w, "upstream URL resolution failed", http.StatusInternalServerError)
		return
	}

	upstreamReq, ureqErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if ureqErr != nil {
		http.Error(w, "build upstream request: "+ureqErr.Error(), http.StatusInternalServerError)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Mcp-Session-Id", "MCP-Protocol-Version"} {
		if v := r.Header.Get(h); v != "" {
			upstreamReq.Header.Set(h, v)
		}
	}
	if upstreamReq.Header.Get("Content-Type") == "" {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}

	upstreamResp, doErr := httpClient.Do(upstreamReq)
	if doErr != nil {
		if isTimeoutErr(doErr) {
			_ = auditFn("warn", "serena-upstream-timeout", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"upstream_url":  upstreamURL,
				"timeout_secs":  int(upstreamTimeout / time.Second),
				"err":           doErr.Error(),
			})
			http.Error(w, fmt.Sprintf("upstream serena daemon at port %d did not respond within %ds", ws.Port, int(upstreamTimeout/time.Second)), http.StatusGatewayTimeout)
			return
		}
		_ = auditFn("warn", "serena-upstream-unreachable", map[string]any{
			"workspace_key": ws.WorkspaceKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"err":           doErr.Error(),
		})
		http.Error(w, fmt.Sprintf("upstream serena daemon at port %d unreachable: %s", ws.Port, doErr.Error()), http.StatusBadGateway)
		return
	}
	defer upstreamResp.Body.Close()

	copyHeaders(w.Header(), upstreamResp.Header)

	contentType := upstreamResp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	w.WriteHeader(upstreamResp.StatusCode)

	if isSSE {
		streamSSE(w, upstreamResp.Body)
		return
	}

	_, _ = io.Copy(w, upstreamResp.Body)
}

// defaultUpstreamURL points at the workspace's serena daemon. Per the
// plan: http://localhost:<workspace.Port>/mcp.
func defaultUpstreamURL(ws *api.WorkspaceEntry) string {
	if ws == nil || ws.Port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", ws.Port)
}

// copyHeaders threads upstream -> downstream headers, skipping the
// connection-management headers that must NOT cross a proxy boundary.
// Hop-by-hop list per RFC 7230 section 6.1.
func copyHeaders(dst, src http.Header) {
	hopByHop := map[string]struct{}{
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"transfer-encoding":   {},
		"upgrade":             {},
	}
	for k, vv := range src {
		if _, hop := hopByHop[strings.ToLower(k)]; hop {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// streamSSE copies from src to dst with explicit flush after each
// read so event-stream frames reach the client without buffering. On
// a writer that does not support http.Flusher, the function degrades
// to plain io.Copy.
func streamSSE(dst http.ResponseWriter, src io.Reader) {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// isTimeoutErr returns true when err is a context-deadline or net
// timeout. Used to pick 504 vs 502 on http.Client.Do failures.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// InMemorySessionRouter is a process-local *api.WorkspaceEntry binding
// map. It exists so test code and production callers can wire a
// working SessionRouter without depending on Agent A1's
// serena_routing package directly; the public surface matches
// sessionRouter exactly.
type InMemorySessionRouter struct {
	mu       sync.RWMutex
	sessions map[string]*api.WorkspaceEntry
}

func newInMemorySessionRouter() *InMemorySessionRouter {
	return &InMemorySessionRouter{sessions: map[string]*api.WorkspaceEntry{}}
}

func (s *InMemorySessionRouter) BindSession(sessionID string, ws *api.WorkspaceEntry) {
	if sessionID == "" || ws == nil {
		return
	}
	s.mu.Lock()
	s.sessions[sessionID] = ws
	s.mu.Unlock()
}

func (s *InMemorySessionRouter) LookupSession(sessionID string) *api.WorkspaceEntry {
	if sessionID == "" {
		return nil
	}
	s.mu.RLock()
	ws := s.sessions[sessionID]
	s.mu.RUnlock()
	return ws
}

func (s *InMemorySessionRouter) UnbindSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// Compile-time guard.
var _ sessionRouter = (*InMemorySessionRouter)(nil)
