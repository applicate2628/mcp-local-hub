// Package mcp is a minimal, self-contained MCP (Model Context Protocol)
// stdio server for the mcp-cbuild binary. It implements the JSON-RPC 2.0
// framing and the MCP basic/lifecycle surface (initialize,
// notifications/initialized, ping, tools/list, tools/call) over stdin/stdout,
// and dispatches tools/call requests to a registered set of Tool
// implementations.
//
// Transport rule (hard): stdout carries ONLY JSON-RPC frames. Every log or
// diagnostic line MUST go to stderr — a stray stdout write corrupts the
// protocol. Callers wire log.SetOutput(os.Stderr) before Serve.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// JSON-RPC 2.0 error codes used by this server (a subset of the spec plus the
// MCP conventions mirrored from the hub's HTTP handler).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// supportedProtocolVersions is the set of MCP protocol versions this server
// speaks. The newest is advertised by default; a client that requests any of
// these gets its requested version echoed back.
var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// defaultProtocolVersion is advertised in initialize when the client does not
// offer a version this server recognizes.
const defaultProtocolVersion = "2025-06-18"

// shutdownGrace bounds how long Serve waits, after stdin EOF, for in-flight
// tools/call goroutines to observe cancellation and run their deferred cleanup
// (the domain layer's process-tree kill) before returning. It must exceed the
// exec layer's post-kill WaitDelay (8s) so a cancelled build's tree-kill fully
// completes; a tool that still refuses to finish past this bound is abandoned so
// shutdown can never wedge.
const shutdownGrace = 10 * time.Second

// Tool is a single MCP tool exposed by the server. Implementations live in the
// domain package (cbuild); the mcp package knows nothing about CMake or vcpkg.
type Tool interface {
	// Name is the stable tool id used in tools/list and tools/call.
	Name() string
	// Title is an optional human-readable label (may be empty).
	Title() string
	// Description is the agent-facing tool description.
	Description() string
	// InputSchema returns the JSON Schema (as a map) describing the tool's
	// arguments object.
	InputSchema() map[string]any
	// Call executes the tool. args is the raw JSON of the tools/call
	// "arguments" object (may be nil/empty). A returned *ParamError maps to a
	// JSON-RPC -32602 InvalidParams response; any other error maps to a
	// tools/call result with isError=true; a nil error returns the result as a
	// successful structured tools/call result. The returned result SHOULD be a
	// JSON object (map/struct), not a bare array or scalar, so it can populate
	// structuredContent.
	Call(ctx context.Context, args json.RawMessage) (any, error)
}

// ParamError signals invalid tool arguments; the server maps it to a JSON-RPC
// -32602 InvalidParams error rather than a tools/call isError result.
type ParamError struct{ Msg string }

func (e *ParamError) Error() string { return e.Msg }

// NewParamError constructs a ParamError with a formatted message.
func NewParamError(format string, a ...any) *ParamError {
	return &ParamError{Msg: fmt.Sprintf(format, a...)}
}

// Server is a stdio MCP server bound to a fixed tool set.
type Server struct {
	name    string
	version string
	tools   map[string]Tool
	order   []string // registration order for deterministic tools/list

	enc     *json.Encoder
	writeMu sync.Mutex

	mu       sync.Mutex
	inflight map[string]context.CancelFunc

	// wg tracks live tools/call goroutines so Serve can drain them (cancel + wait
	// for their deferred process-tree kill) on stdin EOF before returning.
	wg sync.WaitGroup
}

// NewServer builds a server advertising the given name/version and exposing
// the supplied tools (in the order given).
func NewServer(name, version string, tools []Tool) *Server {
	s := &Server{
		name:     name,
		version:  version,
		tools:    make(map[string]Tool, len(tools)),
		inflight: make(map[string]context.CancelFunc),
	}
	for _, t := range tools {
		if _, dup := s.tools[t.Name()]; dup {
			// A duplicate tool name is a programming error; fail loud to stderr.
			log.Printf("mcp: duplicate tool name %q ignored", t.Name())
			continue
		}
		s.tools[t.Name()] = t
		s.order = append(s.order, t.Name())
	}
	return s
}

// requestEnvelope is the inbound JSON-RPC 2.0 message shape. ID is absent for
// notifications, present (number/string/null) for requests.
type requestEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Serve runs the read/dispatch loop until in reaches EOF or ctx is canceled.
// Requests are decoded one newline-delimited JSON message at a time; tools/call
// requests run on their own goroutine so a long build never blocks ping,
// tools/list, or notifications/cancelled. All writes are serialized.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	s.enc = enc

	ctx, cancelAll := context.WithCancel(ctx)
	// On ANY return from Serve (EOF, read error) cancel every in-flight tools/call
	// AND wait (bounded) for its goroutine to run its deferred process-tree kill.
	// Returning on EOF without this drain lets main exit and orphan a still-running
	// cmake/ninja tree during normal stdio shutdown.
	defer s.drainInflight(cancelAll)

	reader := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			// Copy the line: ReadBytes reuses its buffer on the next call, and
			// tools/call dispatch retains params on a goroutine.
			msg := make([]byte, len(trimmed))
			copy(msg, trimmed)
			s.dispatch(ctx, msg)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}
	}
}

// drainInflight cancels every in-flight tools/call and waits (bounded by
// shutdownGrace) for their goroutines to finish. Each cancelled tool observes ctx
// cancellation, tree-kills its child process group, and runs its deferred
// cleanup; draining here guarantees that cleanup completes BEFORE Serve returns
// (and thus before main exits), so a cmake/ninja tree is never orphaned during a
// normal stdin-EOF shutdown. A tool that will not finish within the grace is
// abandoned so shutdown cannot wedge.
func (s *Server) drainInflight(cancelAll context.CancelFunc) {
	cancelAll()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		log.Printf("mcp: shutdown grace (%s) elapsed with tool call(s) still in flight; abandoning them", shutdownGrace)
	}
}

// dispatch parses one message and routes it. Long-running tools/call requests
// are handed to a goroutine; everything else is answered inline.
func (s *Server) dispatch(ctx context.Context, msg []byte) {
	// Distinguish MALFORMED JSON (-32700 parse error) from well-formed JSON that
	// is not a valid JSON-RPC request object (-32600 invalid request).
	var probe json.RawMessage
	if err := json.Unmarshal(msg, &probe); err != nil {
		s.replyError(json.RawMessage("null"), codeParseError, "parse error: "+err.Error(), nil)
		return
	}
	trimmed := bytes.TrimSpace(probe)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Valid JSON but not a request object (array/batch, scalar): no id to
		// address, reply with a null-id invalid-request error.
		s.replyError(json.RawMessage("null"), codeInvalidRequest, "invalid request: expected a JSON-RPC 2.0 request object", nil)
		return
	}
	var env requestEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		s.replyError(json.RawMessage("null"), codeInvalidRequest, "invalid request: "+err.Error(), nil)
		return
	}

	// id classification: ABSENT => notification; PRESENT-and-null or a
	// non-string/number type => an INVALID request (must be answered, id echoed
	// as null); PRESENT string/number => a valid request id.
	present, valid := idState(env.ID)
	isNotification := !present
	if present && !valid {
		s.replyError(json.RawMessage("null"), codeInvalidRequest, "invalid request: id must be a string or number", nil)
		return
	}

	// jsonrpc must be exactly "2.0". A malformed notification cannot be answered.
	if env.JSONRPC != "2.0" {
		if !isNotification {
			s.replyError(env.ID, codeInvalidRequest, `invalid request: "jsonrpc" must be "2.0"`, nil)
		}
		return
	}

	if env.Method == "" {
		if !isNotification {
			s.replyError(env.ID, codeInvalidRequest, "invalid request: method field required", nil)
		}
		return
	}

	switch env.Method {
	case "initialize":
		if isNotification {
			return // initialize is a request; a notification form is malformed — ignore.
		}
		s.handleInitialize(env.ID, env.Params)
	case "notifications/initialized":
		if !isNotification {
			// Carries an id => it is a request, not the lifecycle notification;
			// method-not-found is the correct answer for the request form.
			s.replyError(env.ID, codeMethodNotFound, "method not found: "+env.Method, nil)
		}
		// Notification form: lifecycle no-op.
	case "notifications/cancelled":
		if !isNotification {
			s.replyError(env.ID, codeMethodNotFound, "method not found: "+env.Method, nil)
			return
		}
		s.handleCancelled(env.Params)
	case "ping":
		if !isNotification {
			s.reply(env.ID, map[string]any{})
		}
	case "tools/list":
		if !isNotification {
			s.reply(env.ID, s.toolsList())
		}
	case "tools/call":
		if isNotification {
			return // A tools/call with no id cannot be answered; ignore.
		}
		s.handleToolsCall(ctx, env.ID, env.Params)
	default:
		if !isNotification {
			s.replyError(env.ID, codeMethodNotFound, "method not found: "+env.Method, nil)
		}
	}
}

func (s *Server) handleInitialize(id, params json.RawMessage) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
		if err := json.Unmarshal(params, &p); err != nil {
			s.replyError(id, codeInvalidParams, "invalid initialize params: "+err.Error(), nil)
			return
		}
	}
	negotiated := defaultProtocolVersion
	if supportedProtocolVersions[p.ProtocolVersion] {
		// Echo a version the client asked for when we speak it.
		negotiated = p.ProtocolVersion
	}
	s.reply(id, map[string]any{
		"protocolVersion": negotiated,
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
			"title":   "C/C++ Build (CMake + vcpkg)",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
	})
}

// toolsList builds the tools/list result in registration order.
func (s *Server) toolsList() map[string]any {
	list := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		entry := map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"inputSchema": t.InputSchema(),
		}
		if title := t.Title(); title != "" {
			entry["title"] = title
		}
		list = append(list, entry)
	}
	return map[string]any{"tools": list}
}

// toolCallParams is the tools/call request params shape.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, id, params json.RawMessage) {
	var p toolCallParams
	if len(params) > 0 && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
		if err := json.Unmarshal(params, &p); err != nil {
			s.replyError(id, codeInvalidParams, "invalid tools/call params: "+err.Error(), nil)
			return
		}
	}
	if p.Name == "" {
		s.replyError(id, codeInvalidParams, "invalid tools/call params: name required", nil)
		return
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		// The METHOD (tools/call) exists; the tool NAME is a bad argument, so
		// this is invalid params (-32602), not method-not-found (-32601).
		s.replyError(id, codeInvalidParams, "unknown tool: "+p.Name, nil)
		return
	}

	callCtx, cancel := context.WithCancel(ctx)
	key := s.registerInflight(id, cancel)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releaseInflight(key, cancel)
		// A panic in a tool handler (or its argument parser) must never take down
		// the whole daemon; recover and surface it as an isError tool result so
		// this one call fails while the server keeps serving.
		defer func() {
			if r := recover(); r != nil {
				s.reply(id, toolResult(errorPayload(fmt.Sprintf("tool %q panicked: %v", p.Name, r)), true))
			}
		}()
		result, err := tool.Call(callCtx, p.Arguments)
		switch {
		case err == nil:
			s.reply(id, toolResult(result, false))
		case isParamError(err):
			s.replyError(id, codeInvalidParams, err.Error(), nil)
		default:
			// A tool execution error (could not run, canceled, internal) is
			// surfaced as an isError result so the client/LLM sees the cause
			// rather than an opaque protocol error.
			s.reply(id, toolResult(errorPayload(err.Error()), true))
		}
	}()
}

// handleCancelled cancels an in-flight tools/call matching params.requestId.
func (s *Server) handleCancelled(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || len(p.RequestID) == 0 {
		return
	}
	key := canonicalID(p.RequestID)
	s.mu.Lock()
	cancel := s.inflight[key]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) registerInflight(id json.RawMessage, cancel context.CancelFunc) string {
	key := canonicalID(id)
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	return key
}

func (s *Server) releaseInflight(key string, cancel context.CancelFunc) {
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	cancel()
}

func (s *Server) reply(id json.RawMessage, result any) {
	s.write(&responseEnvelope{JSONRPC: "2.0", ID: normalizeID(id), Result: result})
}

func (s *Server) replyError(id json.RawMessage, code int, msg string, data any) {
	s.write(&responseEnvelope{JSONRPC: "2.0", ID: normalizeID(id), Error: &rpcError{Code: code, Message: msg, Data: data}})
}

func (s *Server) write(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		log.Printf("mcp: write response: %v", err)
	}
}

// toolResult wraps a domain result into an MCP tools/call result: the object is
// serialized both as text content (for text-only clients) and as
// structuredContent (for clients that consume structured output).
func toolResult(result any, isError bool) map[string]any {
	text, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		text = []byte(fmt.Sprintf("%v", result))
	}
	out := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
		"isError": isError,
	}
	// structuredContent must be an object; only attach it when result is a
	// JSON object (map or struct), never a bare array/scalar.
	if isJSONObject(result) {
		out["structuredContent"] = result
	}
	return out
}

func errorPayload(msg string) map[string]any {
	return map[string]any{"success": false, "error": msg}
}

// --- id helpers --------------------------------------------------------------

// idState classifies a raw JSON-RPC id field: present reports whether an id was
// supplied at all (an ABSENT id denotes a notification); valid reports whether a
// present id is an acceptable type (string or number). A present-but-null id, or
// one of bool/object/array type, is present-but-invalid — the caller answers it
// as an invalid request with a null id rather than treating it as a notification.
func idState(raw json.RawMessage) (present, valid bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false, false // absent => notification
	}
	switch t[0] {
	case '"':
		return true, true // string id
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true, true // number id
	default:
		return true, false // null, true, false, {, [ => present but invalid
	}
}

// normalizeID returns a non-nil RawMessage so a response always carries an id
// field (null when the request id was absent/null).
func normalizeID(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// canonicalID compacts an id's JSON so number/string ids map to a stable key
// for the in-flight registry and cancellation matching.
func canonicalID(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return buf.String()
}

func isParamError(err error) bool {
	var pe *ParamError
	return errors.As(err, &pe)
}

// isJSONObject reports whether v marshals to a JSON object ({...}).
func isJSONObject(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{'
}
