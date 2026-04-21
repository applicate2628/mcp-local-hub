package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

// JSONRPCRequest is the minimal request envelope the lazy proxy forwards.
// The proxy reads the request body from the HTTP handler, parses this shape,
// rewrites ID if needed, and hands it to MCPEndpoint.SendRequest.
type JSONRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is the response envelope returned by MCPEndpoint.
type JSONRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError is the standard JSON-RPC 2.0 error payload.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// MCPEndpoint is the request/response surface the lazy proxy talks to once
// materialization succeeds. Implementations own the subprocess lifetime and
// multiplex concurrent proxy calls onto the stdio channel.
type MCPEndpoint interface {
	// SendRequest writes the request to the backend and blocks until the
	// matching response arrives, the context is canceled, or the backend
	// subprocess dies. Must error after Close/Stop has been called.
	SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error)
	// Close marks the endpoint as unusable for further SendRequest calls.
	// Does not terminate the backend subprocess — that is BackendLifecycle.Stop's
	// responsibility. Safe to call multiple times.
	Close() error
}

// BackendLifecycle is the abstraction the lazy proxy uses to spawn the heavy
// backend on first tools/call. Materialize is idempotent: a second call on
// an already-materialized instance returns the existing endpoint. The lazy
// proxy's singleflight gate ensures only one concurrent caller reaches
// Materialize for a fresh instance.
type BackendLifecycle interface {
	// Kind identifies the backend flavor for telemetry and routing. One of
	// "mcp-language-server" | "gopls-mcp".
	Kind() string
	// Materialize spawns the subprocess and returns a ready MCPEndpoint.
	// ctx bounds startup; a ctx-derived timeout is the caller's responsibility.
	// On a missing wrapper binary, the returned error satisfies IsMissingBinaryErr.
	Materialize(ctx context.Context) (MCPEndpoint, error)
	// Stop terminates the subprocess and all derived resources. Safe to call
	// multiple times; safe to call before Materialize.
	Stop() error
}

// errMissingBinary is the sentinel the lazy proxy inspects via
// IsMissingBinaryErr to decide between LifecycleMissing and LifecycleFailed.
var errMissingBinary = errors.New("missing binary")

// IsMissingBinaryErr reports whether err resulted from exec.LookPath failing
// on the wrapper binary. Used by the lazy proxy's state classifier.
func IsMissingBinaryErr(err error) bool {
	return err != nil && errors.Is(err, errMissingBinary)
}

// SendRPC writes a JSON-RPC request (with internal id rewrite) to the hosted
// subprocess and blocks until the matching response arrives or the context
// is canceled. Intended for lazy-proxy consumers (backend lifecycle endpoint
// adapter) that need a synchronous request/response primitive without the
// HTTP handler stack. Concurrent callers multiplex via h.pending the same
// way handlePOST does.
//
// On subprocess death or h.Stop(), returns a descriptive error so callers
// can surface "backend died mid-call" to their own clients.
func (h *StdioHost) SendRPC(ctx context.Context, body []byte) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC request: %w", err)
	}
	internalID := h.nextInternalID.Add(1)
	msg["id"] = json.RawMessage(strconv.FormatInt(internalID, 10))
	rewritten, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal rewritten: %w", err)
	}

	respCh := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[internalID] = respCh
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, internalID)
		h.pendingMu.Unlock()
	}()

	if err := h.writeStdin(rewritten); err != nil {
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	select {
	case resp := <-respCh:
		return []byte(resp), nil
	case <-h.done:
		return nil, errors.New("backend host stopped")
	case <-h.childExited:
		return nil, errors.New("backend subprocess exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// --- mcp-language-server stdio impl ------------------------------------------

// McpLanguageServerStdioConfig configures a stdio-hosted
// mcp-language-server subprocess.
type McpLanguageServerStdioConfig struct {
	WrapperCommand string   // "mcp-language-server"
	WrapperArgs    []string // fully pre-composed, including -workspace / -lsp / [-- flags]
	Workspace      string
	Language       string
	LogPath        string
}

type mcpLanguageServerStdio struct {
	cfg  McpLanguageServerStdioConfig
	mu   sync.Mutex
	host *StdioHost
}

// NewMcpLanguageServerStdio returns a BackendLifecycle that spawns
// mcp-language-server over stdio. Flag shape is single-dash
// (-workspace / -lsp). Upstream flags to the wrapped LSP binary are
// passed through the Go-convention "--" separator that mcp-language-server
// accepts; callers compose WrapperArgs fully and we do not parse them here.
func NewMcpLanguageServerStdio(cfg McpLanguageServerStdioConfig) BackendLifecycle {
	return &mcpLanguageServerStdio{cfg: cfg}
}

func (b *mcpLanguageServerStdio) Kind() string { return "mcp-language-server" }

func (b *mcpLanguageServerStdio) Materialize(ctx context.Context) (MCPEndpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != nil {
		return newStdioHostEndpoint(b.host), nil
	}
	if _, err := exec.LookPath(b.cfg.WrapperCommand); err != nil {
		return nil, fmt.Errorf("%w: %s", errMissingBinary, b.cfg.WrapperCommand)
	}
	h, err := NewStdioHost(HostConfig{
		Command:    b.cfg.WrapperCommand,
		Args:       b.cfg.WrapperArgs,
		WorkingDir: b.cfg.Workspace,
		LogPath:    b.cfg.LogPath,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Start(ctx); err != nil {
		return nil, wrapInitErr(err)
	}
	b.host = h
	return newStdioHostEndpoint(h), nil
}

func (b *mcpLanguageServerStdio) Stop() error {
	b.mu.Lock()
	host := b.host
	b.host = nil
	b.mu.Unlock()
	if host == nil {
		return nil
	}
	return host.Stop()
}

// --- gopls-mcp stdio impl ----------------------------------------------------

// GoplsMCPStdioConfig configures a stdio-hosted `gopls mcp` subprocess.
type GoplsMCPStdioConfig struct {
	WrapperCommand string   // "gopls"
	ExtraArgs      []string // defaults to ["mcp"]
	Workspace      string
	LogPath        string
}

type goplsMCPStdio struct {
	cfg  GoplsMCPStdioConfig
	mu   sync.Mutex
	host *StdioHost
}

// NewGoplsMCPStdio returns a BackendLifecycle that spawns `gopls mcp` over
// stdio. The binary starts over stdio without a -listen flag; the
// subprocess's cwd is the workspace so gopls picks up the right go.mod.
func NewGoplsMCPStdio(cfg GoplsMCPStdioConfig) BackendLifecycle {
	return &goplsMCPStdio{cfg: cfg}
}

func (b *goplsMCPStdio) Kind() string { return "gopls-mcp" }

func (b *goplsMCPStdio) Materialize(ctx context.Context) (MCPEndpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != nil {
		return newStdioHostEndpoint(b.host), nil
	}
	if _, err := exec.LookPath(b.cfg.WrapperCommand); err != nil {
		return nil, fmt.Errorf("%w: %s", errMissingBinary, b.cfg.WrapperCommand)
	}
	args := append([]string(nil), b.cfg.ExtraArgs...)
	if len(args) == 0 {
		args = []string{"mcp"}
	}
	h, err := NewStdioHost(HostConfig{
		Command:    b.cfg.WrapperCommand,
		Args:       args,
		WorkingDir: b.cfg.Workspace,
		LogPath:    b.cfg.LogPath,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Start(ctx); err != nil {
		return nil, wrapInitErr(err)
	}
	b.host = h
	return newStdioHostEndpoint(h), nil
}

func (b *goplsMCPStdio) Stop() error {
	b.mu.Lock()
	host := b.host
	b.host = nil
	b.mu.Unlock()
	if host == nil {
		return nil
	}
	return host.Stop()
}

// --- endpoint adapter --------------------------------------------------------

type stdioHostEndpoint struct {
	host   *StdioHost
	closed atomic.Bool
}

func newStdioHostEndpoint(h *StdioHost) *stdioHostEndpoint {
	return &stdioHostEndpoint{host: h}
}

func (e *stdioHostEndpoint) SendRequest(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if e.closed.Load() {
		return nil, errors.New("endpoint closed")
	}
	// Additionally guard against the owning host having been Stop()ed.
	// The host's done channel is closed on Stop; SendRPC will also observe it,
	// but a quick pre-check turns the common "already stopped" case into a
	// clear, immediate error instead of a racy write-then-fail on stdin.
	select {
	case <-e.host.done:
		return nil, errors.New("endpoint closed: backend host stopped")
	default:
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Guard against concurrent Close(): writeStdin on a stopped host surfaces
	// as "write stdin: ..." from SendRPC; we propagate that as-is.
	raw, err := e.host.SendRPC(ctx, body)
	if err != nil {
		return nil, err
	}
	var resp JSONRPCResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

func (e *stdioHostEndpoint) Close() error {
	e.closed.Store(true)
	return nil
}

// wrapInitErr preserves the concrete init error but annotates it so the lazy
// proxy can distinguish startup failures from missing-binary failures. Any
// error here becomes LifecycleFailed (not Missing) since the binary WAS found
// but the handshake or process-start failed afterward.
//
// Uses %w so errors.Is / errors.As keep working across the wrap; the lazy
// proxy's IsMissingBinaryErr classification relies on the unwrap chain, and
// truncation is better handled at the log or registry-write site (MaxLastErrorBytes
// already caps registry LastError to 200 bytes).
func wrapInitErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("backend init: %w", err)
}
