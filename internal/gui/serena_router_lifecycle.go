// internal/gui/serena_router_lifecycle.go
//
// MCP session-lifecycle synthesis for the /serena/mcp router
// (ROUTER-COMPLETION phase; design
// docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md
// §5 / O1(a)).
//
// The non-tool MCP lifecycle (initialize, tools/list, notifications/*,
// ping) is workspace-agnostic: every serena daemon exposes the same MCP
// surface, and the router itself has NO per-client identity (one
// same-origin route, no client branch — design §9 O1(a)). So the router
// synthesizes these methods directly instead of forwarding them to a
// workspace daemon (it cannot — initialize/tools/list carry no path-arg
// and no bound session). Tool calls (params.name + path-arg, or a bound
// session) keep their existing path-routing + upstream-forward unchanged
// in serenaRouterHandler.
package gui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/api"
)

// sanitizeRefusalPath scrubs an untrusted candidate path before it is folded
// into a JSON-RPC refusal message / `data.path` (area-5 gap-a option B). The
// path originates in an MCP tool-call argument (attacker-influenceable), so a
// hostile agent could otherwise smuggle ANSI/OSC escape sequences or C0/C1
// control bytes into the error that a client UI, log viewer, or terminal would
// render — corrupting output or injecting hyperlinks. It mirrors the catalog
// sanitizer posture (internal/cli/marketplace.go sanitizeCatalogField) using
// the SAME single safety predicate api.IsUnsafeMarketplaceTextRune so the
// control/bidi/Trojan-Source rune set stays defined in one owner:
//   - U+001B (ESC) and other C0 controls (<0x20) → a single space (defeats
//     CSI/OSC sequences without dropping the surrounding path text);
//   - DEL/C1 controls and Unicode bidi/line/paragraph separators → '?';
//   - invalid UTF-8 bytes → '?' so a raw C1 byte cannot hide behind a decode
//     failure.
// Everything else (printable ASCII + safe UTF-8) passes through unchanged.
func sanitizeRefusalPath(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('?')
			i++
			continue
		}
		switch {
		case api.IsUnsafeMarketplaceTextRune(r) && r < 0x20:
			b.WriteByte(' ')
		case api.IsUnsafeMarketplaceTextRune(r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// supportedProtocolVersions is the set of MCP protocol versions the
// synthesized initialize will echo back verbatim. Negotiation rule (MCP
// spec, basic/lifecycle): if the client's requested protocolVersion is
// one the server supports, the server MUST respond with the same
// version; otherwise it responds with a version it does support. The
// router is a thin lifecycle front for serena daemons, so it accepts the
// well-known published MCP revisions and falls back to
// defaultProtocolVersion for anything it does not recognize.
var supportedProtocolVersions = map[string]struct{}{
	"2024-11-05": {},
	"2025-03-26": {},
	"2025-06-18": {},
	"2025-11-25": {},
}

// defaultProtocolVersion is returned when the client requests a version
// the router does not recognize (or omits the field). It is the latest
// revision in supportedProtocolVersions.
const defaultProtocolVersion = "2025-11-25"

var serenaToolsListPortLiveFn = func(ctx context.Context, port int) bool {
	if port <= 0 {
		return false
	}
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// supportedProtocolVersionsList returns the supported MCP protocol
// versions as a sorted slice, for the error.data.supported field on an
// initialize rejection (P2 findings 1 + 5). Mirrors the hub handler's
// hubSupportedVersionsList() (internal/api/hub_mcp_handler.go) but derives
// the slice from supportedProtocolVersions directly so a future edit to
// that map cannot leave the advertised list stale. Sorted descending so
// the newest revision the router speaks is listed first (matches the hub's
// newest-first ordering).
func supportedProtocolVersionsList() []string {
	out := make([]string, 0, len(supportedProtocolVersions))
	for v := range supportedProtocolVersions {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// serenaServerInfoName / serenaServerInfoVersion identify the synthesized
// MCP server. Name matches the manifest server name ("serena") so a
// client's serverInfo display is consistent with the daemon it will
// eventually reach; version marks this as the mcphub router front rather
// than a specific serena build (the real serena version is observable on
// a tool call's upstream response).
const (
	serenaServerInfoName    = "serena"
	serenaServerInfoVersion = "mcphub-router"
)

// jsonrpcInvalidParams / jsonrpcInternalError are the JSON-RPC 2.0
// reserved error codes used by the synthesized lifecycle. The empty-pool
// tools/list uses serenaNoWorkspaceCode, a server-defined code in the
// implementation-defined range (-32000..-32099).
const (
	jsonrpcInvalidRequest = -32600
	jsonrpcInvalidParams  = -32602
	jsonrpcInternalError  = -32603
	serenaNoWorkspaceCode = -32001
	// serenaRootNotTrustedCode is the server-defined code (implementation
	// range -32000..-32099) the serena router returns when first-touch
	// auto-register is refused because the resolved workspace root is not a
	// trusted folder (area-5 trust gate, api.ErrSerenaRootNotTrusted). The
	// accompanying error `data` carries `code:"NEEDS_TRUST"` + the sanitized
	// candidate path so a client/UI can offer one-click trust.
	serenaRootNotTrustedCode = -32002
	// serenaDefaultWorkspaceCode identifies a pathless tools/call whose
	// router-minted session has no sticky workspace and cannot use its default.
	serenaDefaultWorkspaceCode = -32003
)

// workspaceLister is the OPTIONAL capability the router uses to pick a
// live serena daemon to proxy a workspace-agnostic tools/list to. The
// production *serena_routing.WorkspaceResolver implements it; a resolver
// that does not is handled gracefully (tools/list then reports the
// empty-pool error). Kept separate from workspaceResolver so the
// path-routing seam stays minimal and a fake resolver in a tool-call
// test need not implement enumeration.
type workspaceLister interface {
	ListWorkspaces() []*api.WorkspaceEntry
}

// toolsListCacheTTL bounds how long a fetched tools/list result is served
// from cache before the router re-proxies to a live daemon. The serena
// tool surface is effectively static for a given serena version, so a
// short TTL keeps repeated client tools/list calls cheap while still
// picking up a serena upgrade within the window.
const toolsListCacheTTL = 5 * time.Minute

// toolsListCacheEntry is one cached cursorless tools/list result plus the
// time it was fetched (for TTL).
type toolsListCacheEntry struct {
	result  json.RawMessage
	fetched time.Time
}

// toolsListCache caches the workspace-agnostic tools/list result (the raw
// JSON-RPC `result` object) fetched from any live serena daemon. The
// cache is process-local and lives on the Server so it survives across
// requests. It is workspace-agnostic by construction (design §2.1 /
// O1(a): one daemon per workspace, identical MCP surface), so a single
// cached entry per protocol version serves every client regardless of
// which workspace daemon answered the fetch.
//
// Finding E: the cache is KEYED BY the session's negotiated protocol
// version. The serena tool surface a daemon advertises can differ across
// protocol revisions, so a payload fetched under 2025-11-25 must not be
// served to a client that initialized with 2025-06-18. Version-keying is
// preferred over a global single entry (which the pre-fix cache used) so
// each negotiated revision gets its own TTL-bounded entry.
type toolsListCache struct {
	mu       sync.Mutex
	byVer    map[string]toolsListCacheEntry
	cacheTTL time.Duration // 0 -> toolsListCacheTTL; overridable in tests
}

// get returns the cached result for protocolVersion and true when a
// non-expired entry exists for that version.
func (c *toolsListCache) get(protocolVersion string, now time.Time) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.cacheTTL
	if ttl <= 0 {
		ttl = toolsListCacheTTL
	}
	entry, ok := c.byVer[protocolVersion]
	if !ok || len(entry.result) == 0 {
		return nil, false
	}
	if now.Sub(entry.fetched) > ttl {
		return nil, false
	}
	out := make(json.RawMessage, len(entry.result))
	copy(out, entry.result)
	return out, true
}

// put stores result as the cached tools/list payload for protocolVersion.
func (c *toolsListCache) put(protocolVersion string, result json.RawMessage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byVer == nil {
		c.byVer = make(map[string]toolsListCacheEntry)
	}
	c.byVer[protocolVersion] = toolsListCacheEntry{
		result:  append(json.RawMessage(nil), result...),
		fetched: now,
	}
}

// invalidate drops any cached entry for protocolVersion (no-op when absent).
// Finding 3: handleToolsList calls this when the workspace pool is empty so a
// catalog cached while a daemon existed cannot be resurrected on a later read
// after the last workspace was unregistered.
func (c *toolsListCache) invalidate(protocolVersion string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byVer, protocolVersion)
}

// writeJSONRPCResult writes a JSON-RPC 2.0 success envelope carrying id
// and result at HTTP 200. id is threaded verbatim from the request (MCP
// requires the response id to match the request id); a nil/absent id is
// emitted as JSON null per JSON-RPC.
func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any, extraHeaders map[string]string) {
	for k, v := range extraHeaders {
		if v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonrpcEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Result:  result,
	})
}

// writeJSONRPCError writes a JSON-RPC 2.0 error envelope at HTTP 200
// (JSON-RPC carries errors in-band; the transport status stays 200 so a
// compliant client parses the error rather than a transport failure).
func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonrpcEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error:   &jsonrpcError{Code: code, Message: message},
	})
}

// writeJSONRPCRawError writes a JSON-RPC 2.0 error envelope at HTTP 200 whose
// `error` member is the RAW error object bytes (code/message/data preserved
// verbatim). It is used by the workspace-agnostic tools/list to FORWARD a
// daemon's well-formed JSON-RPC error (e.g. -32602 for an invalid opaque
// cursor) to the client UNCHANGED (Finding 4) rather than collapsing it to the
// router's generic -32603. The response id is the CLIENT's request id (MCP
// requires the response id to match the request — the client correlates on its
// own id, not the daemon's), threaded via normalizeID like every other writer
// here. A non-object/empty errObj falls back to a generic internal error so the
// client always receives a parseable JSON-RPC error.
func writeJSONRPCRawError(w http.ResponseWriter, id json.RawMessage, errObj json.RawMessage) {
	trimmed := bytes.TrimSpace(errObj)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		writeJSONRPCError(w, id, jsonrpcInternalError, "serena tools/list: upstream returned a malformed error")
		return
	}
	envelope := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"id":      normalizeID(id),
		"error":   trimmed,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		writeJSONRPCError(w, id, jsonrpcInternalError, "serena tools/list: upstream error re-encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// writeJSONRPCErrorStatus writes a JSON-RPC 2.0 error envelope at an
// EXPLICIT HTTP status, with an optional error.data field. It mirrors the
// hub handler's writeJSONRPCErrorStatus
// (internal/api/hub_mcp_handler.go) so the router's initialize validation
// (P2 findings 1 + 5) can return the same status/code/data shapes the hub
// returns. writeJSONRPCError (HTTP 200, no data) remains for the in-band
// error path that does not need a custom status — initialize rejections
// that the spec maps to a transport error (e.g. session-id present →
// HTTP 400) need this status-carrying variant. A nil data is omitted.
func writeJSONRPCErrorStatus(w http.ResponseWriter, id json.RawMessage, httpStatus, code int, message string, data any) {
	errObj := map[string]any{
		"code":    code,
		"message": message,
	}
	if data != nil {
		errObj["data"] = data
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      normalizeID(id),
		"error":   errObj,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(payload)
}

// normalizeID returns a JSON null RawMessage when id is empty so the
// emitted envelope always carries an explicit id field (JSON-RPC allows
// a null id on a response to a request whose id could not be determined).
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// isJSONRPCNotificationID reports whether a JSON-RPC envelope is a
// notification (no response permitted). It returns true ONLY when the
// "id" field is absent from the body entirely (len == 0). An explicit
// `"id": null` token is NOT a notification — MCP §1.5 requires id to be
// a non-null String/Number, so `id:null` is a malformed request, not a
// fire-and-forget event. This mirrors the hub handler's
// isJSONRPCNotificationID byte-for-byte
// (internal/api/hub_mcp_handler.go) so the router and the hub classify
// the same wire shape identically.
func isJSONRPCNotificationID(id json.RawMessage) bool {
	return len(id) == 0
}

// isValidJSONRPCRequestID reports whether a PRESENT JSON-RPC id is a
// valid request id: a non-null JSON String or Number. MCP §1.5 narrows
// the JSON-RPC base spec to FORBID null ids, and JSON-RPC's grammar
// already forbids booleans, arrays, and objects as ids. So `id:null`,
// `id:true`, `id:{}`, and `id:[1]` are malformed requests, not
// notifications and not synthesizable requests — the caller rejects
// them with -32600 Invalid Request.
//
// PRE: id is PRESENT (len > 0); an absent id is a notification, handled
// by isJSONRPCNotificationID before this runs. The router only needs to
// VALIDATE the id shape (it echoes the id verbatim via normalizeID and
// never keys a map on it), so this is a focused String/Number predicate
// rather than the full numeric canonicalizer in
// internal/api/hub_mcp_request_id.go (which exists only because the hub
// uses the id as an in-flight map key). The rejection rules match that
// validator: null/bool/array/object out, string/number in, leading `+`
// and trailing JSON data on a number rejected.
func isValidJSONRPCRequestID(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	// JSON string: starts with a quote. json.Valid confirms the escapes
	// are well-formed (a bare `"foo` would otherwise pass the prefix
	// check yet be invalid JSON).
	if trimmed[0] == '"' {
		return json.Valid(trimmed)
	}
	// Anything else must be a JSON number. UseNumber keeps the raw
	// decimal string (no float64 demotion) and Decode rejects booleans,
	// arrays, and objects. Demand a single value with nothing trailing.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		// A second value, or any non-EOF error (e.g. `1]`), means
		// leftover input — the id is malformed.
		return false
	}
	num, ok := v.(json.Number)
	if !ok {
		// bool / array / object.
		return false
	}
	// json.Number admits a leading `+` the JSON grammar forbids; reject
	// it defensively so the router never echoes a non-spec id.
	return len(num) == 0 || num[0] != '+'
}

type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// initializeResult is the synthesized InitializeResult payload (MCP
// basic/lifecycle). capabilities advertises tools (the router fronts
// serena's tool surface); listChanged stays false because the router
// does not push tool-list-changed notifications.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      map[string]string `json:"serverInfo"`
}

// newMcpSessionID mints a fresh opaque session id (32 hex chars / 16
// random bytes) for a synthesized initialize. The id is returned to the
// client in the Mcp-Session-Id response header; it binds to a workspace
// later, on the first path-bearing tools/call, via the existing
// BindSession path in serenaRouterHandler. crypto/rand failure is
// vanishingly unlikely; on error we fall back to a time-seeded value so
// initialize never fails for a transient RNG hiccup (the id only needs
// process-local uniqueness for the in-memory session map).
func newMcpSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// initializeRawParams pulls the raw `params` object out of an MCP
// initialize body without disturbing the toolBody decode used for tool
// routing. Unlike a best-effort string probe, returning the raw bytes lets
// handleInitialize tell a PRESENT-but-type-mismatched params object (which
// must be a -32602 error, P2 finding 5) apart from an absent/empty one. A
// malformed/absent params field yields a nil RawMessage; the caller then
// treats protocolVersion as absent (also a -32602 error).
func initializeRawParams(body []byte) json.RawMessage {
	var probe struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	return probe.Params
}

// toolsListIsCursorRequest reports whether an MCP tools/list body must
// BYPASS the first-page cache (P2 finding 2 + findings #3 + #2-round-10). The
// cache holds only the cursorless first page; it must be served ONLY for a
// genuine cursorless first-page request and bypassed for anything the daemon
// should validate/page:
//
//   - params ABSENT or JSON null            -> cursorless first page (cache OK)
//   - params PRESENT but NOT an object      -> malformed; bypass so the DAEMON
//     (number/array/string)                    validates/rejects it (round-10
//     finding #2), rather than masking the
//     error with a cached page-one
//   - params is an object, cursor ABSENT    -> cursorless first page (cache OK)
//   - params object, cursor PRESENT (string, -> paginated request; bypass cache
//     incl empty "")                            so the DAEMON validates the
//     opaque token (Finding 1)
//   - params object, cursor WRONG TYPE      -> malformed; bypass (finding #3)
//     (number/array/object/null)
//
// Finding 1 (https://modelcontextprotocol.io/specification/draft/server/utilities/pagination):
// an MCP cursor is an OPAQUE token; an empty string is a VALID cursor value, NOT
// a first-page/end marker. The router's one-shot tools/list cache holds only the
// cursorless first page, so any PRESENT `cursor` key (even `""`) must bypass the
// cache and proxy to a daemon for validation — serving the cached page-one for a
// `cursor:""` request would silently mask whatever the opaque token meant to the
// daemon. Detection: the `cursor` KEY being PRESENT (a JSON string, including the
// empty string) is the cursor signal; only an ABSENT cursor key is cursorless.
//
// History: the pre-#3 probe unmarshalled cursor into a `string` field, so a
// present non-string cursor failed the WHOLE-body unmarshal and the function
// returned cursorless — serving the cached first page for a malformed paging
// request. #3 parsed params.cursor as json.RawMessage. But that probe still
// decoded `params` itself into a STRUCT, so a present-but-NON-OBJECT params
// (`"params":1`, `"params":[]`, `"params":"x"`) failed the whole-body unmarshal
// and again returned cursorless — serving the cached page-one as success
// instead of proxying so the daemon rejects the invalid params (round-10
// finding #2). Parsing `params` as a json.RawMessage first, then branching on
// its JSON shape, detects a present non-object params independently of cursor.
// Finding 1 then tightened the cursor branch: a PRESENT string cursor (any
// value) bypasses; previously only a NON-EMPTY string did, masking `cursor:""`.
func toolsListIsCursorRequest(body []byte) bool {
	var probe struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// Whole body is not parseable JSON — handled elsewhere (the handler
		// already rejected a malformed body); treat as cursorless here.
		return false
	}
	params := bytes.TrimSpace(probe.Params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		// params absent or JSON null: cursorless first page (cache OK).
		return false
	}
	if params[0] != '{' {
		// params PRESENT but NOT an object (number/array/string): malformed for
		// tools/list — bypass the cache so the daemon validates/rejects it
		// (round-10 finding #2), instead of masking it with a cached page-one.
		return true
	}
	// params is an object: inspect cursor.
	var cursorProbe struct {
		Cursor json.RawMessage `json:"cursor"`
	}
	if err := json.Unmarshal(params, &cursorProbe); err != nil {
		// An object whose decode fails (a non-cursor field of an incompatible
		// shape would not error here since cursor is RawMessage; this is
		// defensive) → bypass so the daemon validates.
		return true
	}
	cursor := bytes.TrimSpace(cursorProbe.Cursor)
	if len(cursor) == 0 {
		// cursor KEY absent: cursorless first page (cache OK).
		return false
	}
	// cursor KEY present (any JSON token: string incl "", or a malformed
	// number/array/object/null). Finding 1: the mere PRESENCE of the cursor key
	// makes this a paginated request that must bypass the cache so the daemon
	// validates the opaque token — an empty string is a VALID cursor, not a
	// first-page marker (MCP pagination spec), and a wrong-typed cursor is the
	// daemon's to reject (finding #3). So every present cursor bypasses.
	return true
}

// handleInitialize synthesizes the InitializeResult and mints a session
// id. Workspace-agnostic: no daemon is contacted (design §5) — every
// serena daemon exposes the same lifecycle surface, and the session is
// bound to a concrete workspace only on the first path-bearing
// tools/call.
//
// incomingSessionID is the request's Mcp-Session-Id header. The MCP
// lifecycle forbids a session id on initialize (initialize is what
// ESTABLISHES the session), and the synthesized body + the rawParams
// type/version validation below mirror the hub handler's handleInitialize
// (internal/api/hub_mcp_handler.go) so the router and hub reject the same
// non-compliant clients identically.
func (s *Server) handleInitialize(w http.ResponseWriter, body []byte, tb *toolBody, incomingSessionID string) {
	// Finding 1 (mirror hub handleInitialize): a session id on initialize
	// is invalid — initialize is what mints the session. Reject it instead
	// of echoing the caller-supplied id (which would let a client
	// reinitialize-with-stale-id and keep a prior workspace/daemon
	// binding). The reconcile readiness probe sends NO session-id header,
	// so it is unaffected.
	if incomingSessionID != "" {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"session-id only valid after initialize", nil)
		return
	}

	// Finding 5 (mirror hub handleInitialize): validate params.protocolVersion
	// synchronously BEFORE minting a session. initializeRawParams returns the
	// raw params bytes so a present-but-type-mismatched params object is told
	// apart from an absent/empty protocolVersion.
	//
	// Finding 1 (P2, Codex PR #249 round-2 — NEGOTIATE an unsupported version
	// instead of rejecting it; DELIBERATE divergence from the hub): the MCP
	// lifecycle spec REQUIRES a server to respond with a version it DOES support
	// when it does not support the requested one (version negotiation —
	// supportedProtocolVersions doc + modelcontextprotocol.io basic/lifecycle).
	// The hub aggregator REJECTS because it fronts HETEROGENEOUS daemons and
	// cannot pick one version for all of them; the serena router fronts
	// HOMOGENEOUS serena daemons (same binary, identical MCP surface), so it CAN
	// and SHOULD negotiate down. The three cases:
	//   - PRESENT well-formed string, SUPPORTED  -> echo it (today's behavior).
	//   - PRESENT well-formed string, UNSUPPORTED -> negotiate: respond 200 with
	//     defaultProtocolVersion as result.protocolVersion, and store THAT as the
	//     session's negotiated version. A forward-compatible client offering a
	//     newer revision can then establish a /serena/mcp session without
	//     special-casing this router (the bug this finding fixes).
	//   - MISSING / empty / non-string (type-mismatch) -> still -32602. That is a
	//     MALFORMED request, not merely an unsupported one — distinct from this
	//     finding, so the strict rejection is preserved.
	rawParams := initializeRawParams(body)
	var initParams struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(rawParams) > 0 && string(rawParams) != "null" {
		if uerr := json.Unmarshal(rawParams, &initParams); uerr != nil {
			// A non-string protocolVersion (e.g. a number) fails this string-field
			// unmarshal: that is a malformed params, not an unsupported version.
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
				"invalid initialize params: "+uerr.Error(), nil)
			return
		}
	}
	if initParams.ProtocolVersion == "" {
		// Missing/empty protocolVersion is an invalid-params error
		// (-32602), not a "default to router-preferred" path: a client that
		// omits the field entirely is malformed, distinct from one that offers
		// a well-formed-but-unsupported version (which negotiates below).
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
			"invalid initialize params: protocolVersion required", map[string]any{
				"supported": supportedProtocolVersionsList(),
			})
		return
	}
	// Finding 1: NEGOTIATE rather than reject when the requested version is a
	// well-formed string the router does not support. The negotiated version is
	// the router default for an unsupported request, or the requested version
	// itself when supported. It is what the result advertises AND what is stored
	// as the session's negotiated version (so the tools/list gate, the
	// version-keyed cache, and the tool-call version enforcement all key off the
	// concrete version the session actually settled on).
	negotiatedVersion := initParams.ProtocolVersion
	if _, ok := supportedProtocolVersions[initParams.ProtocolVersion]; !ok {
		negotiatedVersion = defaultProtocolVersion
	}

	sessionID := newMcpSessionID()
	// Record the minted session + its negotiated version in the
	// authoritative router-session registry (P2 findings 4 + 5 + 7). This
	// is the only place a router session is minted; the tools/list gate,
	// the version-keyed tools/list cache, and the tool-call version
	// enforcement all read from here, and the DELETE teardown unbinds it.
	//
	// Finding 2 (P2, Codex PR #249 round-2): store returns the LRU-evicted client
	// session id when the cap forced an eviction. Coordinate that evicted
	// session's downstream sticky + daemon unbind here — AFTER store returns and
	// the store lock is released (coordinateExpiredRouterSessionUnbind touches
	// OTHER stores; doing it under routerSessionStore.mu would risk a
	// lock-ordering deadlock). Without this, an evicted router session whose
	// sticky/daemon bindings survived would have later pathless calls routed as
	// LEGACY, bypassing the negotiated-version checks + coordinated expiry — the
	// same reanimation class the ticker sweep and on-read expiry close. A nil
	// production sessionRouter (deps unwired / test seam) is handled by the
	// coordinator (it then unbinds only the daemon store).
	if evictedID := s.serenaRouterSessions.store(sessionID, negotiatedVersion); evictedID != "" {
		var sticky sessionRouter
		if deps := s.serenaRouterDepsProd(); deps != nil {
			sticky = deps.Sessions
		}
		s.coordinateExpiredRouterSessionUnbind(evictedID, sticky)
	}
	result := initializeResult{
		ProtocolVersion: negotiatedVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo: map[string]string{
			"name":    serenaServerInfoName,
			"version": serenaServerInfoVersion,
		},
	}
	writeJSONRPCResult(w, tb.ID, result, map[string]string{"Mcp-Session-Id": sessionID})
}

// handleNotificationOrPing answers MCP utility/lifecycle methods that
// need no workspace:
//   - notifications/* WITHOUT an id -> HTTP 202 Accepted, empty body
//     (MCP Streamable HTTP: a POST whose payload is only a notification
//     gets 202 if accepted).
//   - notifications/* WITH an id    -> JSON-RPC -32600 error. An id
//     present on a notification method makes it a JSON-RPC request, and
//     the client will block waiting for a response; a notification can
//     never produce one, so we reject the malformed shape loud instead
//     of acknowledging it (mirrors the hub handler's
//     "notifications/* must not include id" rule in
//     internal/api/hub_mcp_handler.go).
//   - ping WITH an id   -> JSON-RPC result {} (MCP basic/utilities/ping).
//   - ping WITHOUT an id -> HTTP 202, empty body (ping-as-notification;
//     writeJSONRPCResult is not used because a notification gets no
//     response envelope — handled by the caller's notification gate).
//
// Returns true when it handled the method.
//
// NOTE: id-less ping is gated to 202 by the caller (serenaRouterHandler)
// before this runs, so the ping branch here only sees id-bearing pings.
func (s *Server) handleNotificationOrPing(w http.ResponseWriter, tb *toolBody) bool {
	switch {
	case tb.Method == "ping":
		// Reaches here only with an id present (the caller's notification
		// gate already 202'd a ping with no id).
		writeJSONRPCResult(w, tb.ID, map[string]any{}, nil)
		return true
	case isNotificationMethod(tb.Method):
		if !isJSONRPCNotificationID(tb.ID) {
			// notifications/* with an id is a JSON-RPC request that can
			// never be answered. Reject -32600 (Invalid Request) so the
			// client sees the protocol error instead of hanging.
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest,
				"invalid request: notifications/* must not include id")
			return true
		}
		// Genuine notification (no id): no JSON-RPC response.
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	return false
}

// isNotificationMethod reports whether method is an MCP notification
// (the "notifications/" namespace). A genuine notification carries no
// id and the server returns 202 with no body; a notifications/* method
// that DOES carry an id is a malformed request (handled by the caller).
func isNotificationMethod(method string) bool {
	const prefix = "notifications/"
	return len(method) >= len(prefix) && method[:len(prefix)] == prefix
}

// handleToolsList answers a workspace-agnostic tools/list. For the
// cursorless first-page request it serves a cached result when fresh;
// otherwise it proxies one tools/list to a live serena daemon (any
// registered workspace — the surface is identical), caches the daemon's
// `result`, and returns it.
//
// Finding D — require a minted/known router session BEFORE any proxy. A
// client that skipped initialize (or sent a random id) must not be able to
// enumerate tools / trigger handshakes / write the cache. The request's
// Mcp-Session-Id must be present AND minted by a prior initialize at this
// router (serenaRouterSessions); missing/unknown → -32600 at HTTP 400
// (mirrors the hub's "session required"/"unknown session" shape). The
// Phase-3 reconcile probe does initialize ONLY (never tools/list), so this
// gate does not affect it.
//
// Finding E — the cache is keyed by the session's negotiated protocol
// version. The negotiated version (from serenaRouterSessions, the same
// source Finding G uses) keys both the cache read and write AND drives the
// upstream fetch, so a client that negotiated 2025-06-18 cannot be served
// a payload fetched under 2025-11-25.
//
// Finding 1 — reject a tools/list whose MCP-Protocol-Version conflicts with
// the session's negotiated version BEFORE the cache read or proxy, mirroring
// the tool-call path's Finding G and the hub's gate 7
// (internal/api/hub_mcp_handler.go). A missing header uses the session
// version (the resolved version keys the cache + drives the fetch). Without
// this gate a session negotiated as 2025-06-18 could enumerate tools under
// 2025-11-25 and succeed while the hub rejects the same non-initialize
// mismatch.
//
// Cursor-bearing requests bypass the cache entirely (P2 finding 2). The
// workspace-agnostic cache holds only the first page, so reading it for
// a `params.cursor` request would return page 1 for every cursor until
// TTL expiry — the client could never page past the first nextCursor and
// might loop on it. A cursor request is proxied straight to a daemon and
// its (page-N) result is NOT written to the first-page cache. Bypass is
// simpler than keying the cache by cursor and matches serena's
// typically-small, mostly-single-page static tool surface.
//
// Empty-pool case (no workspace registered, so no daemon to fetch from):
// returns an explicit JSON-RPC error (serenaNoWorkspaceCode) rather than
// a fabricated empty tool list. Justification: an empty list would be a
// lie a client could cache forever, never seeing tools appear after the
// operator runs `mcphub workspace register`; a fail-loud error is the
// honest, retryable signal consistent with the router's fail-closed
// posture.
func (s *Server) handleToolsList(
	w http.ResponseWriter,
	r *http.Request,
	deps *serenaRouterDeps,
	tb *toolBody,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	auditFn func(level, event string, fields map[string]any) error,
) {
	// Finding D: require a router session minted by a prior initialize. A
	// missing header or an id this router never minted is rejected -32600
	// at HTTP 400 before any daemon proxy / cache write (mirrors the hub's
	// gate-6 session requirement, internal/api/hub_mcp_handler.go). The
	// negotiated version is captured here and used as the Finding E cache
	// key + the upstream fetch version.
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"session-id required (initialize first)", nil)
		return
	}
	// Finding 4: PEEK (no lastSeen refresh) — both the existence gate just
	// below and the version gate further down are PRE-acceptance. A tools/list
	// rejected as unknown OR for a version mismatch must NOT keep the session
	// alive; lastSeen is refreshed via touch only after BOTH gates pass.
	negotiatedVersion, known := s.serenaRouterSessions.peekNegotiatedVersion(sessionID)
	if !known {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"unknown session (initialize first)", nil)
		return
	}

	// Finding 1 (mirror the tool-call path's Finding G — serena_router.go —
	// and the hub's gate 7, internal/api/hub_mcp_handler.go:382-392): for a
	// KNOWN router session the session's NEGOTIATED version is the source of
	// truth, NOT the raw per-request header. A request MCP-Protocol-Version
	// that conflicts with the negotiated version is a "protocol-version
	// mismatch" (-32600 at HTTP 400, the hub's exact wording). A missing
	// header is fine — negotiatedVersion is used for both the version-keyed
	// cache lookup and the upstream fetch below, so an omitted header stays
	// consistent with the tool-call path. The session is always known here
	// (Finding D's gate above already rejected an unknown/missing session),
	// so there is no raw-header fallthrough as there is on the tool-call path.
	if clientProtocolVersion := r.Header.Get("MCP-Protocol-Version"); clientProtocolVersion != "" && clientProtocolVersion != negotiatedVersion {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"protocol-version mismatch", nil)
		return
	}
	// Finding 4: both gates passed — this is an accepted tools/list on a
	// legitimate session, so refresh lastSeen now (touch on the accepted path
	// only; the peek above did not refresh, so a rejected request never did).
	//
	// Round-10 (Finding 1 — re-check liveness AFTER the gates, mirroring the
	// hub's post-gate Touch, internal/api/hub_mcp_handler.go:402-409). The peek
	// above was PRE-gate; the cleanup ticker or a client DELETE can sweep the
	// session between that peek and now. touch refreshes lastSeen ONLY for a
	// still-live binding and reports whether it did. A false return means the
	// session was swept/terminated mid-flight, so ABORT — do NOT proxy a
	// tools/list (which would handshake a daemon + write the cache) for a dead
	// session.
	if !s.serenaRouterSessions.touch(sessionID) {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"session terminated", nil)
		return
	}

	now := time.Now()

	// Round-9 (Finding 3 — check the workspace pool BEFORE serving the
	// cursorless cache). The empty-pool determination is enumerated FIRST so a
	// stale cached catalog is never served once the last workspace is
	// unregistered. The pre-fix order read the cache first and returned the
	// cached tool list on a hit, so a client kept seeing tools for up to the
	// TTL even though no daemon could execute them (the operator had just run
	// `mcphub workspace unregister`). Enumerating the pool up front makes the
	// empty-pool error authoritative over the cache: when the pool is empty we
	// invalidate the now-unservable entry for this version and fall through to
	// the fail-loud empty-pool error, the same honest/retryable signal the
	// no-daemon path uses.
	lister, ok := deps.Resolver.(workspaceLister)
	if !ok {
		// Resolver cannot enumerate workspaces — treat as empty pool.
		writeJSONRPCError(w, tb.ID, serenaNoWorkspaceCode,
			"serena tools/list unavailable: workspace enumeration not supported by this router build; register a workspace and retry")
		return
	}

	entries := lister.ListWorkspaces()
	if len(entries) == 0 {
		// Empty pool: do NOT serve the cache (Finding 3). Invalidate the stale
		// entry for this version so a later request after a workspace is
		// re-registered re-fetches fresh rather than resurrecting it, then
		// fall through to the fail-loud empty-pool error.
		s.serenaToolsListCache.invalidate(negotiatedVersion)
		writeJSONRPCError(w, tb.ID, serenaNoWorkspaceCode,
			"no serena workspace registered: cannot enumerate tools until at least one workspace is registered (run `mcphub workspace register <path>`), then retry tools/list")
		return
	}

	// The pool is non-empty: a fresh cursorless cache hit is servable (a
	// daemon exists to back it). A paginated (cursor-bearing) tools/list
	// bypasses the first-page cache on BOTH read and write (P2 finding 2). The
	// cache read/write are keyed by the session's negotiated protocol version
	// (P2 finding 5).
	isCursorRequest := toolsListIsCursorRequest(body)
	if !isCursorRequest {
		if cached, ok := s.serenaToolsListCache.get(negotiatedVersion, now); ok {
			writeJSONRPCResult(w, tb.ID, json.RawMessage(cached), nil)
			return
		}
	}

	// FIX-4: the cache missed and we are about to fetch the tool catalog from a
	// candidate daemon. If the WHOLE serena pool is idle-stopped (every external
	// port unbound) the fetch loop below would fail every candidate with a
	// transport error and return a permanent "no daemon answered" error — with
	// NO wake, the pool would stay asleep forever (tools/list is the FIRST call
	// after initialize, before any path-bearing tool-call that would otherwise
	// trip the wake at the tool-call site). Wake ONE serena candidate first so a
	// fully-idle pool comes back up. The shared static tool catalog is identical
	// across daemons, so one live daemon is enough for fetchToolsListFromAnyDaemon
	// to answer. A nil WakeIdleFn (partially-wired routing) skips the wake
	// (back-compat). The wake is bounded + detached, mirroring the tool-call wake.
	//
	// The wake returns the set of candidates that must not be probed by the
	// fetch loop: operator-stop refusals and candidates already inside a
	// stop/prune gate when the wake selector reached them. A merely-transient
	// wake error (respawn not ready yet) is NOT terminal — those candidates stay
	// eligible so the existing wake-as-an-optimization posture is preserved (a
	// not-ready daemon may have come up by the time the fetch loop reaches it,
	// or another live daemon answers).
	wakeExcluded := s.wakeOneSerenaCandidateForToolsList(r, deps, entries, auditFn)
	fetchEntries := excludeSerenaToolsListWakeCandidates(entries, wakeExcluded)
	if len(fetchEntries) == 0 {
		// Every candidate was excluded by the wake gate/refusal path: there is
		// no eligible daemon to probe (probing a stopped/pruning port could
		// accept a foreign catalog). Fail loud + retryable, the same honest
		// signal the no-daemon-answered path uses.
		_ = auditFn("info", "serena-tools-list-all-wake-excluded", map[string]any{
			"workspace_count": len(entries),
		})
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError,
			"serena tools/list: every registered workspace daemon is stopped by an operator stop or currently pruning/stopping; start one with `mcphub start` (or the GUI) and retry")
		return
	}

	// Proxy one tools/list to the first live daemon. On a per-daemon
	// failure, try the next so a single dead daemon does not blank the
	// whole tool surface. The handshake + the upstream tools/list POST use
	// the SESSION's negotiated protocol version (P1 findings 1 + 5, and P2
	// finding 5 consistency — not the raw per-request header, which Finding
	// G treats as advisory for a known session). fetchToolsListFromAnyDaemon
	// also DELETEs each one-shot upstream daemon session after the proxy so
	// it does not leak until the daemon's idle expiry (P2 finding C).
	result, upstreamErr, ferr := s.fetchToolsListFromAnyDaemon(
		r.Context(),
		fetchEntries,
		body,
		httpClient,
		upstreamURLFn,
		func(wsKey string) *api.WorkspaceEntry { return s.resolveWorkspaceByKey(deps, wsKey) },
		deps.WakeIdleFn,
		auditFn,
		negotiatedVersion,
		deps.UpstreamTimeout,
	)
	if ferr != nil {
		_ = auditFn("warn", "serena-tools-list-fetch-failed", map[string]any{
			"err":             ferr.Error(),
			"workspace_count": len(entries),
		})
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError,
			fmt.Sprintf("serena tools/list: no registered workspace daemon answered (%v); the daemons may still be starting — retry shortly", ferr))
		return
	}
	if len(upstreamErr) > 0 {
		// Finding 4: a daemon ANSWERED with a well-formed JSON-RPC error (e.g.
		// -32602 for an invalid opaque cursor). Forward it to the client
		// UNCHANGED — it is a client-actionable validation error, not a router
		// fault, and the fetch already stopped trying other daemons. A
		// page-N/error result is NEVER cached (only the cursorless first page is),
		// so there is no cache write here.
		writeJSONRPCRawError(w, tb.ID, upstreamErr)
		return
	}

	// Only the cursorless first page is cacheable, keyed by the negotiated
	// protocol version (P2 finding 5). A cursor request's page-N result
	// must NOT overwrite the first-page cache entry.
	if !isCursorRequest {
		s.serenaToolsListCache.put(negotiatedVersion, result, now)
	}
	writeJSONRPCResult(w, tb.ID, json.RawMessage(result), nil)
}

// wakeOneSerenaCandidateForToolsList wakes an eligible serena candidate
// before a tools/list fetch (FIX-4). It is the tools/list-path counterpart to
// the tool-call wake (serena_router.go) that the lifecycle path otherwise lacks:
// without it, a tools/list against an all-idle pool can never wake a daemon and
// fails permanently. It is best-effort and intentionally narrow:
//
//   - nil WakeIdleFn → no-op (partially-wired routing, back-compat); returns nil.
//   - tries SERENA candidates with a non-empty TaskName + Port (the wake needs
//     both). LSP rows are skipped — the serena tools/list pool is serena-only.
//     One live daemon is enough (shared static catalog).
//   - bounded (30s, covering clear + reconcile-nudge + readiness) and detached
//     from r.Context() (a client disconnect must not abort the supervisor
//     nudge mid-flight), mirroring the tool-call wake posture.
//
// It distinguishes two wake-error classes (r37-2 P2), mirroring the tool-call
// path's terminal handling (serena_router.go), and also skips candidates that
// are already inside a stop/prune gate:
//
//   - ErrWakeRefusedOperatorStop (an operator deliberately stopped that serena
//     daemon) is TERMINAL for that candidate. The candidate's TaskName is added
//     to the returned set so handleToolsList EXCLUDES it from the fetch loop —
//     its registry port must NEVER be probed, because once the operator stopped
//     the daemon a FOREIGN local service could rebind the freed port and the
//     router would otherwise accept + cache that foreign tool catalog (a
//     correctness + trust-boundary defect). The wake loop still CONTINUES to the
//     next eligible candidate (a merely-idle sibling can still be woken).
//   - any OTHER wake error (respawn not ready in time) is NON-TERMINAL: the
//     candidate stays eligible for the fetch loop (it may have come up by then,
//     or a sibling answers). Waking is an OPTIMIZATION that turns a permanent
//     failure into a retryable one; a transient wake error never excludes the
//     candidate nor gates the tools/list response on its own.
//
// The returned map is keyed by TaskName (the same key handleToolsList filters
// entries on); it is nil when no candidate was excluded.
func (s *Server) wakeOneSerenaCandidateForToolsList(
	r *http.Request,
	deps *serenaRouterDeps,
	entries []*api.WorkspaceEntry,
	auditFn func(level, event string, fields map[string]any) error,
) map[string]struct{} {
	if s == nil || deps == nil || deps.WakeIdleFn == nil {
		return nil
	}
	var excluded map[string]struct{}
	excludeCandidate := func(ws *api.WorkspaceEntry) {
		if ws == nil || ws.TaskName == "" {
			return
		}
		if excluded == nil {
			excluded = make(map[string]struct{})
		}
		excluded[ws.TaskName] = struct{}{}
	}
	wakeCtx, wakeCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer wakeCancel()
	for _, ws := range entries {
		if ws == nil || ws.TaskName == "" || ws.Port == 0 {
			continue
		}
		if !isSerenaWorkspaceEntry(ws) {
			continue
		}
		candidateLive := func() bool {
			live := false
			entered, aborted, err := s.withSerenaWorkspaceGate(
				context.Background(),
				ws.WorkspaceKey,
				serenaWorkspaceGatePolicyTryOnly,
				func(string) *api.WorkspaceEntry {
					return ws
				},
				nil,
				func(out *serenaWorkspaceGateOutcome) bool {
					excludeCandidate(ws)
					_ = auditFn("info", "serena-tools-list-wake-candidate-gated", map[string]any{
						"workspace_key":        ws.WorkspaceKey,
						"task_name":            ws.TaskName,
						"port":                 ws.Port,
						"waited_through_prune": out.gate.waitedThroughPrune,
					})
					return true
				},
				func(out *serenaWorkspaceGateOutcome) (err error) {
					candidate := out.ws
					if candidate == nil {
						return
					}
					if out.gate.waitedThroughPrune {
						candidate = s.resolveWorkspaceByKey(deps, ws.WorkspaceKey)
						if candidate == nil || candidate.TaskName == "" || candidate.Port == 0 || !isSerenaWorkspaceEntry(candidate) {
							return
						}
					}
					hadActiveIdleStop := serenaTaskHasActiveIdleStop(candidate.TaskName, time.Now())
					if wakeErr := deps.WakeIdleFn(wakeCtx, candidate.TaskName, candidate.Port, "serena-tools-list-wake"); wakeErr != nil {
						// r37-2 P2: classify the wake error. An operator-stop refusal is
						// TERMINAL — record the TaskName so handleToolsList excludes this
						// candidate from the fetch loop (its freed port must never be probed,
						// or a foreign rebind could poison the cached catalog). The tool-call
						// path applies the same terminal exclusion (serena_router.go). A
						// transient (not-ready) wake error is NON-terminal: leave the
						// candidate eligible so the fetch loop can still try it (back-compat
						// optimization posture). Either way, CONTINUE to the next candidate so
						// a wakeable idle sibling can still satisfy this tools/list.
						if errors.Is(wakeErr, api.ErrWakeRefusedOperatorStop) {
							excludeCandidate(candidate)
							_ = auditFn("info", "serena-tools-list-wake-operator-stopped", map[string]any{
								"workspace_key": candidate.WorkspaceKey,
								"task_name":     candidate.TaskName,
								"port":          candidate.Port,
								"err":           wakeErr.Error(),
							})
							return
						}
						_ = auditFn("info", "serena-tools-list-wake-noted", map[string]any{
							"workspace_key": candidate.WorkspaceKey,
							"task_name":     candidate.TaskName,
							"port":          candidate.Port,
							"err":           wakeErr.Error(),
						})
						return
					}
					if hadActiveIdleStop {
						s.reseedSerenaBackendPIDAfterConfirmedWake(wakeCtx, candidate)
					}
					if serenaToolsListPortLiveFn(wakeCtx, candidate.Port) {
						live = true
						return
					}
					_ = auditFn("info", "serena-tools-list-wake-noted", map[string]any{
						"workspace_key": candidate.WorkspaceKey,
						"task_name":     candidate.TaskName,
						"port":          candidate.Port,
						"err":           "wake returned nil but candidate port is not accepting TCP connections",
					})
					return
				},
			)
			if err != nil {
				return false
			}
			if !entered {
				if aborted {
					return false
				}
				_ = auditFn("info", "serena-tools-list-wake-gate-timeout", map[string]any{
					"workspace_key": ws.WorkspaceKey,
					"task_name":     ws.TaskName,
					"port":          ws.Port,
				})
				return false
			}
			return live
		}()
		if candidateLive {
			return excluded
		}
	}
	return excluded
}

// excludeSerenaToolsListWakeCandidates returns the subset of entries whose
// TaskName was not excluded by the tools/list wake gate/refusal path. The
// excluded set covers operator-stop refusals and candidates already in an
// idle-stop/prune phase when the wake selector reached them; those ports must
// not be probed by fetchToolsListFromAnyDaemon.
func excludeSerenaToolsListWakeCandidates(entries []*api.WorkspaceEntry, excluded map[string]struct{}) []*api.WorkspaceEntry {
	if len(excluded) == 0 {
		return entries
	}
	out := make([]*api.WorkspaceEntry, 0, len(entries))
	for _, ws := range entries {
		if ws != nil && ws.TaskName != "" {
			if _, skip := excluded[ws.TaskName]; skip {
				continue
			}
		}
		out = append(out, ws)
	}
	return out
}

// isClientToolsListError classifies an upstream tools/list JSON-RPC error
// code as a CLIENT/request-level error (the request itself is invalid, so
// EVERY daemon would reject it identically) vs a SERVER-side error (this one
// daemon is unhealthy, but another registered daemon could still answer the
// shared static catalog). It reads the JSON-RPC error `code`:
//
//   - CLIENT (true) → -32700 parse error, -32600 invalid request,
//     -32601 method not found, -32602 invalid params. Per JSON-RPC 2.0 §5.1
//     these are all faults of the REQUEST; retrying the same body against a
//     different workspace daemon is pointless, so the caller short-circuits and
//     forwards the error to the client (Finding 3 + the round-11 #4 behavior
//     for request-level errors).
//   - SERVER (false) → -32603 internal error AND the server-reserved range
//     -32000..-32099 (JSON-RPC §5.1 "reserved for implementation-defined
//     server-errors"). One daemon's internal failure must NOT blank the whole
//     tool surface, so the caller treats it as a candidate failure and falls
//     through to the next daemon (Finding 3 — the bug round-11 #4 introduced by
//     short-circuiting on ALL well-formed errors).
//
// An unknown/out-of-range code (e.g. an application-defined positive code) is
// treated CONSERVATIVELY as a client error (short-circuit + forward): it is not
// in the server-reserved range, so it is most likely request-specific, and
// forwarding the daemon's own error to the client is more honest than masking
// it by trying other daemons. The classification is intentionally narrow — only
// the JSON-RPC-defined server codes fall through.
func isClientToolsListError(code int) bool {
	switch code {
	case -32700, -32600, -32601, -32602:
		return true
	}
	// JSON-RPC server-reserved range (-32000..-32099) + the -32603 internal
	// error are SERVER errors → not client errors.
	if code == jsonrpcInternalError {
		return false
	}
	if code >= -32099 && code <= -32000 {
		return false
	}
	// Anything else (application-defined / unknown) → conservatively client.
	return true
}

// toolsListErrorCode extracts the JSON-RPC error `code` from a well-formed
// upstream error envelope's error OBJECT bytes (the proxyToolsListResult
// .upstreamError). It returns (code, true) on a parseable object with an
// integer code, else (0, false) — a malformed/codeless error object cannot be
// classified, so the caller treats it conservatively (see the call site).
func toolsListErrorCode(errObj json.RawMessage) (int, bool) {
	var e struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(errObj, &e); err != nil || e.Code == nil {
		return 0, false
	}
	return *e.Code, true
}

// fetchToolsListFromAnyDaemon forwards the verbatim tools/list body to
// each candidate workspace daemon in turn, returning the first daemon's
// JSON-RPC `result`. It validates that the daemon answered HTTP 200 with
// a JSON-RPC result (not an error); a non-result answer advances to the
// next candidate. Returns the last error when every candidate fails.
//
// clientProtocolVersion is threaded to each proxyToolsListOnce so the
// handshake + tools/list POST carry the version the client negotiated
// (P1 findings 1 + 5).
//
// Finding C: the tools/list handshake mints a fresh ONE-SHOT upstream
// daemon session (the router does not reuse it for later tool calls — those
// re-handshake via serenaDaemonSessions). proxyToolsListOnce returns the
// daemon-issued session id so this loop can best-effort DELETE it after the
// proxy, on BOTH the success path AND the post-handshake error path
// (otherwise the session leaks until the daemon's idle expiry). The DELETE
// is context-detached (a finished/cancelled tools/list request context must
// not abort the teardown), mirroring the main DELETE path.
//
// Finding 4 (round-11) + Finding 3 (this round — CLASSIFY the error): a
// candidate that ANSWERS with a well-formed JSON-RPC error envelope
// (upstreamError) is AVAILABLE and rejecting THIS request — it is NOT a
// transport failure. Round-11 #4 short-circuited on ANY such error (return it
// immediately, try no other daemons). That is RIGHT for a CLIENT/request-level
// error (-32700/-32600/-32601/-32602): the request is invalid for every daemon,
// so forward it unchanged and do not retry. But it was WRONG for a SERVER error
// (-32603 internal, or the server-reserved -32000..-32099 range): one unhealthy
// daemon returning an internal error must NOT prevent trying other registered
// daemons that could answer the shared static catalog. Finding 3 classifies via
// isClientToolsListError(code): a CLIENT error short-circuits + forwards (the
// round-11 behavior, now scoped to request-level codes); a SERVER error is
// treated as a candidate FAILURE — the loop captures it as lastUpstreamError
// and falls through to the next daemon. When EVERY candidate fails with a server
// error, the loop returns the LAST server error envelope (forwarded to the
// client, preserving the signal) rather than the generic transport error. Only a
// genuine transport/handshake failure (the `err` return) is recorded in lastErr.
func (s *Server) fetchToolsListFromAnyDaemon(
	ctx context.Context,
	entries []*api.WorkspaceEntry,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	resolveWorkspaceByKeyFn func(wsKey string) *api.WorkspaceEntry,
	wakeIdleFn func(context.Context, string, int, string) error,
	auditFn func(level, event string, fields map[string]any) error,
	clientProtocolVersion string,
	upstreamTimeout time.Duration,
) (result json.RawMessage, upstreamError json.RawMessage, err error) {
	var lastErr error
	// Finding 3: the most recent SERVER-side JSON-RPC error envelope (-32603 or
	// the -32000..-32099 range). A server error does NOT short-circuit; it is a
	// candidate failure that falls through to the next daemon. When every
	// candidate fails with a server error, this is returned so the client gets
	// the real signal rather than the generic "no daemon answered".
	var lastUpstreamError json.RawMessage
	for _, ws := range entries {
		if ws == nil {
			continue
		}
		upstreamURL := upstreamURLFn(ws)
		if upstreamURL == "" {
			lastErr = fmt.Errorf("workspace %q: upstream URL resolution failed", ws.WorkspaceKey)
			continue
		}
		// Finding 3 (round-10): pass the upstream timeout so proxyToolsListOnce
		// bounds the handshake + tools/list (possibly SSE) body reads — a daemon
		// that opens an SSE stream but never sends a complete response event no
		// longer hangs this candidate forever, so the loop can advance.
		outcome, _, _, perr := func() (proxyToolsListResult, string, string, error) {
			gateCtx, gateCancel := upstreamReadContext(ctx, upstreamTimeout)
			defer gateCancel()
			var outcome proxyToolsListResult
			var daemonSessionID string
			var daemonProtocolVersion string
			var perr error
			var gateErr error
			entered, aborted, err := s.withSerenaWorkspaceGate(
				gateCtx,
				ws.WorkspaceKey,
				serenaWorkspaceGatePolicyBlock,
				func(wsKey string) *api.WorkspaceEntry {
					if resolveWorkspaceByKeyFn == nil {
						gateErr = fmt.Errorf("workspace %q: pruned during stop gate wait", wsKey)
						return nil
					}
					candidate := resolveWorkspaceByKeyFn(wsKey)
					if candidate == nil {
						gateErr = fmt.Errorf("workspace %q: pruned during stop gate wait", wsKey)
					}
					return candidate
				},
				upstreamURLFn,
				func(out *serenaWorkspaceGateOutcome) bool {
					candidate := out.ws
					if wakeIdleFn == nil || candidate == nil || candidate.TaskName == "" || candidate.Port == 0 {
						return false
					}
					hadActiveIdleStop := serenaTaskHasActiveIdleStop(candidate.TaskName, time.Now())
					if err := wakeIdleFn(gateCtx, candidate.TaskName, candidate.Port, "serena-tools-list-rewake"); err != nil {
						gateErr = fmt.Errorf("workspace %q: re-wake after idle-stop: %w", candidate.WorkspaceKey, err)
						return true
					}
					if hadActiveIdleStop {
						s.reseedSerenaBackendPIDAfterConfirmedWake(gateCtx, candidate)
					}
					out.rewoke = true
					return false
				},
				func(out *serenaWorkspaceGateOutcome) (err error) {
					candidate := out.ws
					upstreamURL := out.upstreamURL
					if candidate == nil {
						if gateErr != nil {
							perr = gateErr
							return
						}
						perr = fmt.Errorf("workspace %q: pruned during stop gate wait", ws.WorkspaceKey)
						return
					}
					if upstreamURL == "" {
						perr = fmt.Errorf("workspace %q: upstream URL resolution failed", candidate.WorkspaceKey)
						return
					}
					outcome, daemonSessionID, daemonProtocolVersion, perr = proxyToolsListOnce(ctx, httpClient, upstreamURL, body, clientProtocolVersion, upstreamTimeout)
					// Finding C: release the one-shot upstream session regardless of the
					// proxy outcome, whenever the handshake established one. Detach the
					// context so a done tools/list request context cannot abort it.
					//
					// Invariant D (#4): bound the teardown by the SHORT cleanup budget,
					// NOT serenaUpstreamTimeout (60s). This teardown runs synchronously
					// before the tools/list result returns to the client, so a hung daemon
					// would otherwise delay the client's tools/list by up to a minute per
					// candidate. cleanupContext keeps it detached (Finding C) AND bounded
					// to <=5s (or the configured upstreamTimeout when shorter, for test
					// determinism).
					//
					// Finding #8 + Finding 2 (V-forward): the teardown DELETE carries the
					// version the DAEMON negotiated for this one-shot session (the version
					// it was established + advanced under), not merely the requested
					// version, so a strict daemon that binds the header to its session's
					// initialized version does not 400 the teardown and leak the one-shot
					// session. effectiveHandshakeProtocolVersion fills the router default
					// only if the daemon returned an empty version.
					if daemonSessionID != "" {
						// Invariant D (#4): the teardown still uses the SHORT cleanup budget
						// (cleanupContext derives min(serenaCleanupTimeout, upstreamTimeout)),
						// independent of the longer Finding-3 read deadline on the proxy above.
						delCtx, delCancel := cleanupContext(upstreamTimeout)
						s.forwardSerenaDeleteUpstream(delCtx, httpClient, upstreamURL, daemonSessionID, effectiveHandshakeProtocolVersion(daemonProtocolVersion), candidate.WorkspaceKey, candidate, auditFn)
						delCancel()
					}
					return
				},
			)
			if err != nil {
				return proxyToolsListResult{}, "", "", err
			}
			if !entered {
				return proxyToolsListResult{}, "", "", fmt.Errorf("workspace %q: stop gate wait exceeded tools/list budget", ws.WorkspaceKey)
			}
			if aborted {
				if gateErr != nil {
					return proxyToolsListResult{}, "", "", gateErr
				}
				return proxyToolsListResult{}, "", "", fmt.Errorf("workspace %q: stop gate aborted tools/list candidate", ws.WorkspaceKey)
			}
			return outcome, daemonSessionID, daemonProtocolVersion, perr
		}()
		if perr != nil {
			// Genuine transport/handshake failure (daemon unavailable): record it
			// and advance to the next candidate.
			lastErr = fmt.Errorf("workspace %q (port %d): %w", ws.WorkspaceKey, ws.Port, perr)
			_ = auditFn("warn", "serena-tools-list-daemon-skip", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"err":           perr.Error(),
			})
			continue
		}
		if len(outcome.upstreamError) > 0 {
			// Finding 4 + Finding 3: the daemon answered with a well-formed
			// JSON-RPC error. Classify it by the JSON-RPC `code`. A CLIENT error
			// (-32700/-32600/-32601/-32602, or an unclassifiable/codeless envelope
			// treated conservatively as client) is the same for every daemon — the
			// request is invalid — so forward it to the client UNCHANGED and try no
			// other daemons (round-11 #4). A SERVER error (-32603 or -32000..-32099)
			// is THIS daemon's fault, not the request's — capture it and fall
			// through so a healthier daemon can still answer the shared catalog
			// (Finding 3 — the regression round-11 #4 introduced).
			code, hasCode := toolsListErrorCode(outcome.upstreamError)
			if !hasCode || isClientToolsListError(code) {
				_ = auditFn("info", "serena-tools-list-daemon-error", map[string]any{
					"workspace_key": ws.WorkspaceKey,
					"port":          ws.Port,
					"jsonrpc_code":  code,
					"classified":    "client",
				})
				return nil, outcome.upstreamError, nil
			}
			// Server error: record it as a candidate failure and advance.
			lastUpstreamError = outcome.upstreamError
			_ = auditFn("warn", "serena-tools-list-daemon-server-error", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"jsonrpc_code":  code,
				"classified":    "server",
			})
			continue
		}
		return outcome.result, nil, nil
	}
	// Finding 3: every candidate failed. Prefer the LAST server-side JSON-RPC
	// error envelope (a real daemon-reported signal) over the generic transport
	// error, so the client sees WHY the daemons could not answer rather than an
	// opaque "no daemon answered". Fall back to the transport lastErr only when
	// no daemon returned a server error (every failure was a transport/handshake
	// fault).
	if len(lastUpstreamError) > 0 {
		return nil, lastUpstreamError, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate workspace daemon available")
	}
	return nil, nil, lastErr
}

// proxyToolsListOnce POSTs the tools/list body to one daemon's /mcp
// endpoint and extracts the JSON-RPC `result`. It accepts both a plain
// JSON body and a single-event SSE body (serena daemons may answer either
// per the MCP Streamable HTTP transport). A 1 MiB read cap bounds the
// response.
//
// It returns a proxyToolsListResult carrying EITHER the daemon's `result`
// (nextCursor stripped — Finding 2) OR the daemon's well-formed JSON-RPC
// `error` object (Finding 4). The error return is reserved for a genuine
// transport/handshake failure (daemon unavailable); a daemon that ANSWERS with
// a JSON-RPC error is a successful round-trip whose error object is carried in
// the result struct so the caller forwards it unchanged instead of skipping to
// other daemons.
//
// Before the tools/list POST it performs the MCP handshake with the
// daemon (initialize → notifications/initialized) and sends the
// daemon-issued Mcp-Session-Id on the tools/list request (P1). A healthy
// serena / native-http daemon requires a session for every non-initialize
// POST, so a bare tools/list with no preceding initialize would be
// rejected with a 400 / JSON-RPC session error. A handshake failure (dead
// daemon) surfaces as the per-candidate error so fetchToolsListFromAnyDaemon
// advances to the next daemon. A sessionless daemon yields an empty
// session id, in which case tools/list is sent with no Mcp-Session-Id
// (back-compat).
//
// clientProtocolVersion (P1 findings 1 + 5) is the version the client put
// on the tools/list request's MCP-Protocol-Version header (empty for an
// older client). The handshake AND the tools/list POST use the SAME
// resolved version so the daemon's session is initialized at the version
// the tools/list header advertises — a strict daemon binding the header
// to the session's initialized version (see
// internal/api/hub_mcp_handler.go gate 7) rejects a non-initialize POST
// whose header is missing OR mismatched. The header is omitted only when
// the resolved version is empty (cannot happen — the fallback default is
// non-empty).
//
// Finding C: it returns the daemon-issued session id (the second return
// value) so the caller can DELETE the one-shot session afterwards. The id
// is returned on every path AFTER a successful handshake — including the
// post-handshake error paths — so the caller tears it down even when the
// tools/list POST itself fails. A handshake FAILURE returns "" (no upstream
// session was minted, so there is nothing to release); a sessionless daemon
// also returns "" (it issued no id).
func proxyToolsListOnce(ctx context.Context, httpClient *http.Client, upstreamURL string, body []byte, clientProtocolVersion string, upstreamTimeout time.Duration) (outcome proxyToolsListResult, daemonSessionID string, daemonProtocolVersion string, err error) {
	requestedVersion := effectiveHandshakeProtocolVersion(clientProtocolVersion)
	// Finding 3 (round-10): bound the handshake reads by the upstream timeout
	// (passed through to establishDaemonSession's read context).
	daemonSessionID, daemonProtocolVersion, hsErr := establishDaemonSession(ctx, httpClient, upstreamURL, requestedVersion, upstreamTimeout)
	if hsErr != nil {
		return proxyToolsListResult{}, "", "", fmt.Errorf("upstream handshake: %w", hsErr)
	}
	// Finding #8: send the tools/list POST under the version the DAEMON
	// negotiated for this session (a strict daemon binds the header on a
	// non-initialize POST to its session's initialized version, which may
	// differ from the requested version). Fall back to the requested version
	// if the daemon was sessionless / omitted its version.
	postVersion := daemonProtocolVersion
	if postVersion == "" {
		postVersion = requestedVersion
	}
	// Finding 3 (round-10): bound the tools/list POST + its (possibly SSE) body
	// read by the upstream timeout. A daemon that answers tools/list with SSE
	// headers but no complete response event would otherwise hang
	// readUpstreamJSONRPCResponse forever (the handshake's own deadline context
	// has already been released by establishDaemonSession's defer cancel). The
	// derived deadline makes the transport abort the body read so this returns
	// an error and fetchToolsListFromAnyDaemon advances to the next daemon.
	postCtx, postCancel := upstreamReadContext(ctx, upstreamTimeout)
	defer postCancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Accept both shapes; daemons that only emit SSE need the stream
	// Accept, plain-JSON daemons ignore the extra type.
	req.Header.Set("Accept", "application/json, text/event-stream")
	// tools/list is a non-initialize POST: send the negotiated protocol
	// version so a strict daemon does not reject the missing header
	// (P1 finding 5 + Finding #8 — the daemon-negotiated version).
	if postVersion != "" {
		req.Header.Set("MCP-Protocol-Version", postVersion)
	}
	if daemonSessionID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Finding 3 (P2, Codex PR #249 round-2 — preserve an upstream JSON-RPC error
	// on a NON-2xx tools/list): a serena daemon that rejects a tools/list with a
	// JSON-RPC error at HTTP 400 (e.g. an invalid cursor/params — the SAME status
	// the router uses for its own -32602) ANSWERED with a well-formed JSON-RPC
	// error. The pre-fix non-200 guard returned a transport error BEFORE reading
	// the body, so fetchToolsListFromAnyDaemon treated it as a daemon TRANSPORT
	// failure (tries other daemons → generic -32603), discarding a
	// client-actionable error. We now read the body on a non-200 too and, when it
	// carries a parseable JSON-RPC error, surface that error object so the caller
	// runs the SAME isClientToolsListError classification as the 200 path (a
	// CLIENT error like -32602 is forwarded + short-circuits other daemons; a
	// SERVER error falls through to the next daemon). Only a non-200 with NO
	// parseable JSON-RPC error body (a genuine transport/HTTP failure — e.g. a 5xx
	// with no JSON body, or an unreadable body) stays a transport failure so the
	// loop advances.
	is200 := resp.StatusCode == http.StatusOK
	// Finding 4: read INCREMENTALLY (same ReadAll-to-EOF hang as the
	// handshake — a daemon that answers tools/list as a still-open SSE
	// stream would otherwise block until the client timeout).
	// readUpstreamJSONRPCResponse returns at the first JSON-RPC response
	// event for SSE, or a bounded read for application/json.
	payload, rerr := readUpstreamJSONRPCResponse(resp.Header.Get("Content-Type"), resp.Body)
	if rerr != nil {
		if !is200 {
			// Finding 3: a non-200 whose body could not even be read is a genuine
			// transport/HTTP failure (e.g. a 5xx that hangs or resets) — surface the
			// status so the caller tries the next daemon, not the unread-body error.
			return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("upstream tools/list -> status %d", resp.StatusCode)
		}
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("read upstream tools/list: %w", rerr)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if uerr := json.Unmarshal(payload, &rpc); uerr != nil {
		if !is200 {
			// Finding 3: a non-200 with a non-JSON (or non-JSON-RPC) body is a
			// genuine transport/HTTP failure (e.g. a 502 + an HTML error page). Keep
			// it a transport failure so the loop advances to the next daemon.
			return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("upstream tools/list -> status %d", resp.StatusCode)
		}
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("upstream tools/list non-JSON-RPC body: %w", uerr)
	}
	if len(rpc.Error) > 0 {
		// Finding 4 + Finding 3: the daemon ANSWERED with a well-formed JSON-RPC
		// error envelope (e.g. -32602 for an invalid opaque cursor) — on a 200 OR a
		// non-2xx status (a daemon may pair a JSON-RPC error with HTTP 400). The
		// daemon IS available and is rejecting THIS request — it is NOT a transport
		// failure. Surface the raw error object so fetchToolsListFromAnyDaemon
		// CLASSIFIES it (isClientToolsListError) and either forwards it to the
		// client UNCHANGED (client error → no other daemons) or falls through to the
		// next daemon (server error), instead of masking it with the router's
		// generic -32603. (err stays nil: a daemon-level rejection is a successful
		// round-trip, not a candidate transport failure.)
		return proxyToolsListResult{upstreamError: rpc.Error}, daemonSessionID, daemonProtocolVersion, nil
	}
	if !is200 {
		// Finding 3: a non-200 whose body is valid JSON-RPC but carries NO error
		// (and, below, no usable result) is still a transport/HTTP failure — the
		// daemon did not answer the request. Surface the status so the loop advances.
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("upstream tools/list -> status %d", resp.StatusCode)
	}
	if len(rpc.Result) == 0 {
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("upstream tools/list returned no result")
	}
	// Finding 2: strip any pagination cursor (result.nextCursor) before the
	// router returns/caches the result. The router's workspace-agnostic
	// tools/list uses a ONE-SHOT upstream session (handshake → tools/list →
	// DELETE), so it cannot honor a follow-up opaque cursor across sessions
	// (cursors are session-scoped — a page-2 request would hit a FRESH daemon
	// session that never issued that cursor). Presenting the surface as a single
	// complete page and never advertising a cursor it cannot honor is the
	// simplest correct option for serena (whose tool surface is static and
	// single-page — a no-op there) and lossy-but-safe for a hypothetical
	// paginating daemon (page 1 becomes the surface, no broken cross-session
	// cursor loop). The `tools` array and every other result field are preserved.
	stripped, serr := stripToolsListNextCursor(rpc.Result)
	if serr != nil {
		return proxyToolsListResult{}, daemonSessionID, daemonProtocolVersion, fmt.Errorf("strip tools/list nextCursor: %w", serr)
	}
	return proxyToolsListResult{result: stripped}, daemonSessionID, daemonProtocolVersion, nil
}

// proxyToolsListResult is the parsed outcome of one upstream tools/list proxy.
// Exactly one of result / upstreamError is set on a successful round-trip:
//   - result        : the daemon's JSON-RPC `result` (with nextCursor stripped,
//     Finding 2) — the surface to return/cache.
//   - upstreamError  : the daemon's well-formed JSON-RPC `error` object
//     (Finding 4) — the daemon answered and is rejecting THIS request; forward
//     it to the client unchanged rather than trying other daemons.
//
// A transport/handshake failure (daemon unavailable) is signalled by the
// separate error return of proxyToolsListOnce, NOT this struct.
type proxyToolsListResult struct {
	result        json.RawMessage
	upstreamError json.RawMessage
}

// stripToolsListNextCursor removes the `nextCursor` key from a tools/list
// JSON-RPC result object and re-marshals the rest (Finding 2). The result is
// decoded into an ordered-key-agnostic map[string]json.RawMessage so every
// other field (notably `tools`) is preserved byte-faithfully; only the
// pagination cursor is dropped. A result that is not a JSON object (a daemon
// bug) is returned unchanged — the strip is a no-op rather than an error so a
// non-paginating daemon is never penalised. When no nextCursor key is present
// (the serena case) the original bytes are returned unchanged.
func stripToolsListNextCursor(result json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(result)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// Not a JSON object (or empty): nothing to strip.
		return result, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["nextCursor"]; !ok {
		// No pagination cursor (the serena single-page case): return verbatim so
		// the result is byte-identical to what the daemon sent.
		return result, nil
	}
	delete(fields, "nextCursor")
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// bytesContainsFold is a tiny case-insensitive substring check for
// content-type matching (avoids importing strings just for ToLower in
// this file; the value is already lowercased by most servers but we
// fold defensively).
func bytesContainsFold(haystack, needle string) bool {
	return bytes.Contains(bytes.ToLower([]byte(haystack)), bytes.ToLower([]byte(needle)))
}

// readUpstreamJSONRPCResponse reads a daemon's upstream MCP response body
// and returns the JSON-RPC envelope bytes to parse, bounded by a 1 MiB
// defensive cap. It is the read used by the synthesized handshake
// (postHandshakeInitialize) and the workspace-agnostic tools/list proxy
// (proxyToolsListOnce) — both expect a single JSON-RPC RESPONSE.
//
// Finding 4 — for a text/event-stream body it parses the stream
// INCREMENTALLY and returns as soon as it assembles the FIRST complete
// JSON-RPC response event, WITHOUT waiting for the stream to close. A
// Streamable-HTTP daemon may keep its SSE connection open after emitting
// the response event (to push later notifications); the pre-fix code did
// io.ReadAll(resp.Body) to EOF and then flattened with extractJSONRPCPayload,
// so the handshake blocked until the client timeout (60s) on EVERY first
// tool-call / tools-list against such a daemon. This mirrors the hub
// aggregator's readSSEResponse + selectJSONRPCResponse
// (internal/api/hub_mcp_aggregator.go) — those helpers are unexported in
// package api (the router lives in package gui), so the technique is
// replicated here rather than imported. A non-SSE (application/json)
// body keeps a bounded read (io.ReadAll under the same cap).
func readUpstreamJSONRPCResponse(contentType string, body io.Reader) ([]byte, error) {
	const maxBytes = 1 << 20
	if bytesContainsFold(contentType, "text/event-stream") {
		return readSSEJSONRPCResponse(body, maxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxBytes))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// readSSEJSONRPCResponse scans a text/event-stream body line-by-line and
// returns the data of the FIRST complete event whose payload is a JSON-RPC
// RESPONSE envelope (jsonrpc=="2.0" with a non-empty `id` and no `method`).
// It stops at that event without draining the stream to EOF (Finding 4), so
// a daemon that holds the connection open after the response does not stall
// the handshake. It mirrors internal/api/hub_mcp_aggregator.go's
// readSSEResponse byte-for-byte in technique (bufio.Scanner with a maxBytes
// buffer; blank line dispatches an event; non-data fields and `:` comments
// are skipped; multiple `data:` lines within one event join with "\n"; one
// optional leading space stripped from each value); notification events
// (id absent / method present) are skipped so the caller keeps reading for
// the actual response. maxBytes caps total bytes so a runaway daemon cannot
// OOM the router.
func readSSEJSONRPCResponse(r io.Reader, maxBytes int) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	// Allow a single line up to maxBytes — some daemons emit one large
	// `data:` line carrying the full JSON envelope.
	scanner.Buffer(make([]byte, 64*1024), maxBytes+1)

	var dataLines [][]byte
	totalBytes := 0
	for scanner.Scan() {
		raw := scanner.Bytes()
		totalBytes += len(raw) + 1 // +1 for the LF the scanner consumed
		if totalBytes > maxBytes {
			return nil, fmt.Errorf("SSE response too large (> %d bytes)", maxBytes)
		}
		line := bytes.TrimSuffix(raw, []byte("\r"))

		if len(line) == 0 {
			// Blank line: dispatch the accumulated event (SSE boundary).
			if len(dataLines) > 0 {
				if payload, ok := selectSSEJSONRPCResponse(dataLines); ok {
					return payload, nil
				}
				dataLines = nil
			}
			continue
		}
		if line[0] == ':' {
			// SSE comment line — ignored.
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			// Other SSE field (`event:`, `id:`, `retry:`, unknown).
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		// Copy: the scanner reuses its buffer on the next Scan, so retaining
		// the slice across iterations would corrupt the accumulated event.
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		dataLines = append(dataLines, valueCopy)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE: %w", err)
	}
	// Stream ended without a trailing blank line — dispatch any final event.
	if len(dataLines) > 0 {
		if payload, ok := selectSSEJSONRPCResponse(dataLines); ok {
			return payload, nil
		}
	}
	return nil, errors.New("SSE stream ended without a JSON-RPC response event")
}

// selectSSEJSONRPCResponse joins the accumulated `data:` lines of one SSE
// event with "\n" and returns the payload IFF it is a JSON-RPC RESPONSE
// envelope (jsonrpc=="2.0", non-empty `id`, no `method`). Notifications
// (id absent, method present) and unrelated events are rejected so the
// caller keeps reading. Mirrors internal/api/hub_mcp_aggregator.go's
// selectJSONRPCResponse.
func selectSSEJSONRPCResponse(dataLines [][]byte) ([]byte, bool) {
	payload := bytes.Join(dataLines, []byte("\n"))
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false
	}
	if env.JSONRPC != "2.0" {
		return nil, false
	}
	// JSON-RPC responses ALWAYS carry an `id` and never a `method`.
	if len(env.ID) == 0 || env.Method != "" {
		return nil, false
	}
	return payload, true
}
