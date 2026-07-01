// hub_mcp_handler.go — Phase 4 Task 4.2 (G4 unified hub MCP).
//
// HubMcpHandler implements the 7-check auth gate + JSON-RPC dispatch
// for /clients/{client}/mcp (POST + DELETE + GET fallback).
//
// Auth gate ordering (spec §"Cross-client invariant — seven-check auth
// gate"):
//
//   1. Loopback-guard: Host, Origin, Sec-Fetch-Site (rejects DNS-rebind
//      + cross-site fetch).
//   2. Path → canonical client_id. Unknown → 404 empty body.
//   3. Token shape: 64-lowercase-hex header. Anything else → 401.
//   4. Constant-time token compare via ConstantTimeCompareToken.
//   5. Instance-id match: X-Mcphub-Instance-Id == endpoint.InstanceID
//      (constant-time compare).
//   6. Session-client binding: every non-initialize POST + every DELETE
//      requires Mcp-Session-Id; the session's Client field MUST equal
//      the path client_id. initialize REQUIRES the header to be ABSENT.
//      GET is exempt from this check (codex r7-bot-r5 P2 closure).
//   7. MCP-Protocol-Version validation: header required on every method
//      OTHER than initialize and GET (codex r7-bot-r6 P2 closure).
//      Value must equal the session's negotiated version AND be in
//      hubSupportedVersions. Mismatch → 400 with JSON-RPC -32600.
//
// Every 401 returns an identical empty body — no oracle. The
// JSON-RPC -32600 body is used only for the version-mismatch case
// (400) and for the initialize-time unsupported-version response.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Cross-client invariant" + §"Client-origin lifecycle methods" +
// §"Tool-name namespacing".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 4.2.

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/clients"
)

// hubSupportedVersions enumerates the MCP protocol versions the hub
// accepts during initialize negotiation. Add new versions as we test
// against new clients; remove old versions only after confirming no
// in-repo or known external caller still emits them. The current set
// keeps `2025-03-26` because internal callers in
// internal/daemon/backend_lifecycle.go + internal/api/health.go +
// internal/api/register.go still emit it (codex r7-bot-r5 P1 closure).
var hubSupportedVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
}

const hubMcpResponseWriteTimeout = 30 * time.Second

// hubSupportedVersionsList returns a deterministic slice of the
// supported versions (sorted newest-first) for inclusion in
// JSON-RPC error responses' data.supported field.
func hubSupportedVersionsList() []string {
	return []string{"2025-11-25", "2025-06-18", "2025-03-26"}
}

// HubMcpHandler holds the live atomic-pointer references to the hub's
// instance state. Construction goes through NewHubMcpHandler so callers
// can wire it onto the listener mux in internal/gui/hub_listener.go.
type HubMcpHandler struct {
	// instanceEndpoint is the persisted endpoint record, captured at
	// hub bind time. The handler reads InstanceID at every request for
	// gate 5 (instance-id match). Stored as atomic.Pointer so a future
	// rotation path (Phase 5) can swap it without restarting the hub.
	instanceEndpoint atomic.Pointer[HubEndpoint]
	sessions         *HubSessionStore
}

// NewHubMcpHandler returns a handler ready to mount on
// /clients/{client}/mcp. The session store owns aggregator-side state
// (per-session route maps, in-flight requests); the handler delegates
// every aggregator call to the store's hubSessions.
//
// SetEndpoint must be called once after construction to publish the
// instance-id used by gate 5. Calling SetEndpoint with a nil/zero
// HubEndpoint is a programming error — gate 5 would reject every
// request with 401.
func NewHubMcpHandler(store *HubSessionStore) *HubMcpHandler {
	return &HubMcpHandler{sessions: store}
}

// SetEndpoint publishes the active endpoint state. Called by
// internal/gui/hub_listener.go after EnsureHubEndpoint succeeds; the
// handler reads InstanceID from this snapshot at every request.
func (h *HubMcpHandler) SetEndpoint(ep HubEndpoint) {
	cpy := ep
	h.instanceEndpoint.Store(&cpy)
}

// currentInstanceID returns the published endpoint's InstanceID, or
// "" if SetEndpoint has not yet been called. Used by gate 5 and to seed
// initialize-time session metadata; tools/list responses read the session
// cached InstanceID so metadata does not depend on a later endpoint read.
func (h *HubMcpHandler) currentInstanceID() string {
	p := h.instanceEndpoint.Load()
	if p == nil {
		return ""
	}
	return p.InstanceID
}

// ServeHTTP routes every /clients/{client}/mcp request through the
// 7-check auth gate. GET goes to the 405-with-Allow fallback after
// gates 1-5. POST and DELETE require gates 1-7 in order.
//
// Spec §"Cross-client invariant" — checks 1-5 run for every method,
// 6-7 for POST + DELETE only.
func (h *HubMcpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Gate 1: loopback-guard. Rejects DNS-rebind via Host/Origin and
	// cross-site fetch via Sec-Fetch-Site (mirrors
	// internal/daemon/loopback_guard.go — duplicated here because
	// internal/daemon imports internal/api, so we cannot import the
	// other direction without a cycle).
	if !isSafeLoopbackHubRequest(r) {
		http.Error(w, "forbidden loopback request", http.StatusForbidden)
		return
	}

	// Gate 2: path → kind-namespaced scope key. Two prefixes are
	// served by this same handler (groups/namespaces Phase 4b):
	//   - /clients/<id>/mcp  → kind=client, scopeKey = bare <id>
	//   - /g/<group>/mcp      → kind=group,  scopeKey = "g:"+<group>
	// The client path stays BYTE-IDENTICAL to before: a client kind is
	// gated by isSupportedClient and its scope key is the bare id, so
	// every downstream comparison (token row, sess.ScopeKey, bindings
	// lookup) is unchanged for clients.
	kind, name, ok := parseHubPathFromURL(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	scopeKey, known := resolveHubScopeKey(kind, name)
	if !known {
		// Unknown path or unknown client/group → 404 with EMPTY body so
		// the failure shape is identical regardless of whether the path
		// is well-formed-but-unknown vs malformed.
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// GET fallback (codex r7-bot-r5 P2 closure): runs gates 3-5 (token
	// + instance id) but is EXEMPT from gates 6-7 (no Mcp-Session-Id
	// or MCP-Protocol-Version requirement). MCP Streamable HTTP 2025-
	// 11-25 mandates 405 + `Allow: POST, DELETE` for GET on the endpoint.
	//
	// Issue #159 protocol lane #4 closure: an EXPLICIT invalid
	// MCP-Protocol-Version header MUST return 400 even on GET — the
	// spec says version validation precedes method-not-allowed. An
	// empty/absent header still returns 405 (no implied version).
	if r.Method == http.MethodGet {
		if !h.checkTokenAndInstanceID(w, r, scopeKey) {
			return
		}
		if pv := r.Header.Get("MCP-Protocol-Version"); pv != "" && !hubSupportedVersions[pv] {
			body, _ := json.Marshal(map[string]any{
				"error":     "unsupported MCP-Protocol-Version",
				"requested": pv,
				"supported": hubSupportedVersionsList(),
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Gates 3-5: token shape + constant-time compare + instance id.
	if !h.checkTokenAndInstanceID(w, r, scopeKey) {
		return
	}

	// Gates 6-7 + dispatch live in per-method handlers.
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r, scopeKey)
	case http.MethodDelete:
		h.handleDelete(w, r, scopeKey)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// checkTokenAndInstanceID runs gates 3-5: token-shape, constant-time
// token compare, instance-id match. Returns true iff all three pass.
// Failure paths write a 401 with EMPTY body — no oracle on the
// failed gate.
//
// Spec §"Cross-client invariant" steps 3-5: "Anything else → 401
// (identical body)" — the response is intentionally bare so an
// attacker cannot distinguish wrong-shape vs wrong-token vs
// wrong-instance.
func (h *HubMcpHandler) checkTokenAndInstanceID(w http.ResponseWriter, r *http.Request, scopeKey string) bool {
	tok := r.Header.Get("X-Mcphub-Hub-Token")
	// Gate 3: shape (64 lower-hex).
	if !isLowerHex64(tok) {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	// Gate 4: constant-time compare. Returns 1 only when the table has
	// a 64-hex entry for scopeKey AND it matches tok byte-for-byte. The
	// scope key is the bare client id for a /clients/ request and the
	// "g:<group>" key for a /g/ request; the token table holds a row for
	// both kinds (clients via EnsureHubTokens, groups via
	// EnsureGroupTokens), so the compare is kind-agnostic.
	if ConstantTimeCompareToken(scopeKey, tok) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	// Gate 5: instance id match. Constant-time compare against the
	// published endpoint's InstanceID. An empty currentInstanceID
	// (handler not yet wired) defeats every request — that's the
	// correct fail-closed posture.
	wantID := h.currentInstanceID()
	hdr := r.Header.Get("X-Mcphub-Instance-Id")
	if len(hdr) == 0 || len(wantID) == 0 || subtle.ConstantTimeCompare([]byte(hdr), []byte(wantID)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

// handlePost runs gates 6-7 (session + protocol version), then
// dispatches the JSON-RPC method via aggregator entry points. scopeKey is
// the kind-namespaced binding key parsed from the URL — a bare client id
// for /clients/, or "g:<group>" for /g/ (groups/namespaces Phase 4b).
func (h *HubMcpHandler) handlePost(w http.ResponseWriter, r *http.Request, scopeKey string) {
	// Body-size cap: 4 MiB matches maxAggregatorResponseBytes (spec
	// §"Concurrency + bounds" — same ceiling on inbound + outbound).
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAggregatorResponseBytes+1))
	if err != nil {
		writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32700, "parse error: "+err.Error(), nil)
		return
	}
	if len(body) > maxAggregatorResponseBytes {
		writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32700, "request too large", nil)
		return
	}

	// JSON-RPC envelope parse. Method + id + params extracted via
	// raw-message round-trip so id preserves its number-vs-string
	// discriminator verbatim.
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #2):
		// json.Unmarshal returns errors for BOTH invalid-JSON-text
		// (true -32700 Parse error) AND valid-JSON-with-type-mismatch
		// (e.g. {"jsonrpc": 42} — should be -32600 Invalid Request).
		// We can't distinguish here without re-parsing, but the
		// pragmatic rule per JSON-RPC §5.1 is: if it's a json.SyntaxError
		// → -32700, else → -32600. Distinguish via errors.As.
		var syntaxErr *json.SyntaxError
		if errors.As(uerr, &syntaxErr) {
			writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32700, "parse error: "+uerr.Error(), nil)
		} else {
			writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32600, "invalid request: "+uerr.Error(), nil)
		}
		return
	}

	// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #2):
	// Validate id FIRST so subsequent error response paths can
	// safely echo it. The pre-r24 order had jsonrpc!="2.0" check
	// BEFORE id validation, so `{"jsonrpc":"1.0","id":true}` would
	// echo back `"id": true` in the error envelope — MCP §1.5
	// violation (id MUST be string|number, and JSON-RPC says
	// invalid id MUST be echoed as null).
	//
	// Earlier closures kept:
	// - r19 P2 closure: explicit `id: null` is invalid under MCP
	// - r22 P2 closure: route through newRequestIDKey for full
	//   id-type validation (rejects null, bool, array, object,
	//   empty). Same canonical validator the in-flight tracker uses.
	respID := env.ID
	if !isJSONRPCNotificationID(env.ID) {
		if _, idErr := newRequestIDKey(env.ID); idErr != nil {
			writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32600, idErr.Error(), nil)
			return
		}
	}

	// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #2):
	// jsonrpc!="2.0" is an invalid-request-shape error, not a
	// parse error. -32600 per JSON-RPC §5.1.
	if env.JSONRPC != "2.0" {
		writeJSONRPCErrorStatus(w, respID, http.StatusBadRequest, -32600, "invalid request: jsonrpc must be \"2.0\"", nil)
		return
	}
	// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #2):
	// missing method field = -32600 Invalid Request, not -32601
	// (Method not found is for a valid-name-but-unknown method).
	if env.Method == "" {
		writeJSONRPCErrorStatus(w, respID, http.StatusBadRequest, -32600, "invalid request: method field required", nil)
		return
	}

	// initialize is its own branch — Mcp-Session-Id MUST be absent
	// and version validation runs at initialize-time (codex r7-bot-r2
	// P2 closure: no half-initialized session if version is rejected).
	if env.Method == "initialize" {
		// codex bot phase4 r20 P2 closure on PR #158: notification-
		// shaped initialize ({"method":"initialize"} without id) MUST
		// be rejected — initialize is the session-establishment
		// handshake, not a fire-and-forget event. Allowing it would
		// let a malformed client allocate session slots until the
		// idle-sweeper kicks in, eventually starving legitimate
		// clients at the session cap. -32600 surfaces the client bug
		// instead of silently swallowing the request as a noop.
		if isJSONRPCNotificationID(env.ID) {
			writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32600, "invalid request: initialize requires a non-null id", nil)
			return
		}
		h.handleInitialize(w, r, scopeKey, env.ID, env.Params)
		return
	}

	// Gate 6: session required for every non-initialize POST.
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		// Empty body per spec: "400 with empty body" for the missing-
		// header case. We do not emit a JSON-RPC error body — clients
		// that omit the header have not negotiated a session, so they
		// have no id to echo back anyway.
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// codex bot phase4 r25 P2 closure on PR #158: split the atomic
	// GetAndTouch (introduced in r24) back into Get + post-gate
	// Touch. r24's combined operation refreshed LastUsedAt BEFORE
	// the cross-client + protocol-version gates, so a malicious or
	// buggy client could keep a session alive past its idle TTL by
	// spamming requests with mismatched clientID or wrong
	// MCP-Protocol-Version — every failed request still bumped
	// LastUsedAt. The original r24 P1 (sweep-Delete race between
	// Get and Touch) is still addressed: Touch returns bool, and
	// the handler now checks it AFTER gates pass; if the session
	// was swept mid-request, the handler aborts with the same
	// "unknown session" path. Detection via bool is the correct
	// minimal fix; atomicity was overengineering.
	sess, ok := h.sessions.Get(sid)
	if !ok {
		// codex bot phase4 r21 P2 closure on PR #158: notification-
		// shaped requests (e.g. notifications/cancelled) MUST NOT
		// receive a response — they can legitimately arrive AFTER
		// session teardown (client cancels in-flight request while
		// the hub idle-sweeper just GC'd the session). Strict MCP
		// clients treating an unexpected response object as a
		// protocol error would otherwise drop subsequent valid
		// responses. Real requests still get the -32600 envelope so
		// the client can see why it was rejected.
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Unknown session → JSON-RPC -32600 body with HTTP 404.
		writeJSONRPCErrorStatus(w, env.ID, http.StatusNotFound, -32600, "unknown session", nil)
		return
	}
	if sess.ScopeKey != scopeKey {
		// Cross-scope session reuse → 401 empty body (no oracle). The
		// kind-namespaced key makes a group session replayed under a
		// client path (or vice-versa) unequal here, so the existing
		// cross-client fence now also fences cross-KIND reuse for free.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Gate 7: MCP-Protocol-Version. Header is REQUIRED on every method
	// other than initialize + GET (codex r7-bot-r6 P2 closure).
	pv := r.Header.Get("MCP-Protocol-Version")
	if pv == "" {
		// Missing header → 400 with empty body. Protocol-level error
		// (not auth), and the client has not provided an id we can
		// safely echo back.
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !hubSupportedVersions[pv] || pv != sess.ProtocolVersion {
		// codex bot phase4 r21 P2 closure on PR #158: same rule —
		// notifications don't get a JSON-RPC response. A version
		// mismatch on a notification path is unusual but stays
		// silent to preserve notification semantics.
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSONRPCErrorStatus(w, env.ID, http.StatusBadRequest, -32600, "protocol-version mismatch", nil)
		return
	}

	// codex bot phase4 r25 P2 closure on PR #158: refresh activity
	// timestamp ONLY after all gates pass (auth + protocol-version).
	// Touch returns false if the session was swept between the
	// Get above and now — close the r24 P1 race window (sweep
	// Delete vs handler keeping a stale *hubSession): treat the
	// false return as an "unknown session" outcome for this
	// request, abort. Notifications stay silent per r21.
	if !h.sessions.Touch(sid) {
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSONRPCErrorStatus(w, env.ID, http.StatusNotFound, -32600, "unknown session", nil)
		return
	}

	// Method dispatch.
	switch env.Method {
	case "notifications/initialized":
		// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #2):
		// JSON-RPC §4.1 + MCP §1.5: a notification MUST omit id. If
		// id IS present, this is a request with a notification-style
		// method name — reject as invalid request (-32600). The
		// pre-r24 code accepted any envelope with this method as a
		// no-response notification, silently swallowing the
		// presence-of-id protocol bug.
		if !isJSONRPCNotificationID(env.ID) {
			writeJSONRPCErrorStatus(w, env.ID, http.StatusBadRequest, -32600, "invalid request: notifications/* must not include id", nil)
			return
		}
		// No body, just 202. The aggregator does not fan-out
		// notifications/initialized at this stage — it was already
		// dispatched per-daemon during initialize fan-out (see
		// hub_mcp_aggregator.go AggregateInitialize). The client's
		// initialized signal here just unblocks subsequent method
		// calls on the hub side.
		w.WriteHeader(http.StatusAccepted)
		return
	case "notifications/cancelled":
		// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #2):
		// same notification-must-not-include-id rule.
		if !isJSONRPCNotificationID(env.ID) {
			writeJSONRPCErrorStatus(w, env.ID, http.StatusBadRequest, -32600, "invalid request: notifications/* must not include id", nil)
			return
		}
		// Parse params.requestId; forward to the matching daemon
		// in-flight row via ForwardCancellation.
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if uerr := json.Unmarshal(env.Params, &p); uerr != nil {
			// Malformed notification — silently 202 per JSON-RPC
			// notification semantics (no response is mandated).
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// codex bot phase4 r15 P2 closure on PR #158: derive cancel
		// fan-out ctx from context.Background(), NOT r.Context().
		// A client that disconnects right after sending the
		// notifications/cancelled message would cancel r.Context()
		// immediately, short-circuiting ForwardCancellation before
		// it issues the daemon-side cancel — the daemon-side call
		// would then keep running until its own timeout. Same r7 P2
		// pattern that closed the DELETE fan-out leak. The
		// PerCallWallClockCap budget remains; we just stop letting
		// the client's disconnect steer the cancel-forwarding path.
		ctx, cancel := context.WithTimeout(context.Background(), PerCallWallClockCap)
		defer cancel()
		ForwardCancellation(ctx, sess, p.RequestID)
		w.WriteHeader(http.StatusAccepted)
		return
	case "ping":
		// Hub-local echo: empty result. writeJSONRPCResult handles
		// the notification-no-response case (r17 P2 closure) so a
		// ping notification gets HTTP 202 with empty body.
		writeJSONRPCResult(w, env.ID, map[string]any{})
		return
	case "tools/list":
		// codex bot phase4 r18 P2 closure on PR #158: tools/list as
		// a JSON-RPC notification (no id) MUST NOT receive a response
		// body. The aggregator would otherwise emit an envelope with
		// id:null, which strict MCP clients treat as a protocol error
		// (server responded to a notification). Same r17 P2 pattern.
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		respBody, aerr := AggregateToolsList(r.Context(), sess, env.ID, h.currentInstanceID())
		if aerr != nil {
			writeJSONRPCErrorStatus(w, env.ID, http.StatusInternalServerError, -32603, "internal error: "+aerr.Error(), nil)
			return
		}
		writeRawJSON(w, respBody)
		return
	case "tools/call":
		// codex bot phase4 r18 P2 closure on PR #158: same
		// notification-no-response rule for tools/call.
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		respBody, aerr := AggregateToolsCall(r.Context(), sess, env.ID, env.Params)
		if aerr != nil {
			writeJSONRPCErrorStatus(w, env.ID, http.StatusInternalServerError, -32603, "internal error: "+aerr.Error(), nil)
			return
		}
		writeRawJSON(w, respBody)
		return
	default:
		// codex bot phase4 r18 P2 closure (extension on PR #158):
		// unknown methods arriving as notifications also stay silent
		// per JSON-RPC 2.0 §4.1 (server MUST NOT respond to a
		// notification, even with an error envelope).
		if isJSONRPCNotificationID(env.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSONRPCErrorStatus(w, env.ID, http.StatusOK, -32601, "Method not found: "+env.Method, nil)
		return
	}
}

// handleInitialize is the initialize sub-handler. Mcp-Session-Id MUST
// be absent; protocolVersion is validated synchronously before any
// session is created (codex r7-bot-r2 P2 closure). scopeKey is the
// kind-namespaced binding key (bare client id for /clients/, "g:<group>"
// for /g/) — it becomes sess.ScopeKey and selects the snapshot bindings
// the new session fans out to (groups/namespaces Phase 4b).
func (h *HubMcpHandler) handleInitialize(w http.ResponseWriter, r *http.Request, scopeKey string, reqID json.RawMessage, paramsRaw json.RawMessage) {
	if r.Header.Get("Mcp-Session-Id") != "" {
		writeJSONRPCErrorStatus(w, reqID, http.StatusBadRequest, -32600, "session-id only valid after initialize", nil)
		return
	}
	var initParams struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	// codex bot phase4 r16 P2 closure on PR #158: reject
	// syntactically-valid-but-type-mismatched params.
	if len(paramsRaw) > 0 && string(paramsRaw) != "null" {
		if uerr := json.Unmarshal(paramsRaw, &initParams); uerr != nil {
			writeJSONRPCErrorStatus(w, reqID, http.StatusBadRequest, -32602, "invalid initialize params: "+uerr.Error(), nil)
			return
		}
	}
	// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #2):
	// MCP §1.6 Lifecycle requires the client to send a supported
	// protocolVersion in `initialize` params. Missing/empty
	// protocolVersion is an invalid-params error (-32602), not a
	// "default to hub-preferred" path. Silently defaulting masks
	// non-compliant clients that omit the field.
	if initParams.ProtocolVersion == "" {
		writeJSONRPCErrorStatus(w, reqID, http.StatusBadRequest, -32602, "invalid initialize params: protocolVersion required", map[string]any{
			"supported": hubSupportedVersionsList(),
		})
		return
	}
	// MCP §1.6 Lifecycle says servers SHOULD respond with another
	// supported version when the offered one is unknown. SHOULD,
	// not MUST. For a HUB that aggregates tools from many daemons,
	// silently downgrading to "2025-11-25" when the client offered
	// e.g. "1900-01-01" hides a real client/server mismatch — the
	// client expected its requested version's features and now gets
	// a different protocol, leading to tool-discovery failures
	// further down the line.
	//
	// Issue #159 closure (handler-initialize-unsupported-version):
	// reject unsupported versions outright with -32600 +
	// error.data.supported enumerating what the hub speaks. The
	// client sees the rejection synchronously and can re-issue
	// `initialize` with a supported version. The r7-bot-r2 P2
	// closure (test pinned by
	// TestHandlerInitializeUnsupportedVersionReturnsSyncJSONRPCError)
	// already required this; the r24 "negotiate" path broke the
	// test contract. Reverting to strict rejection — also documented
	// in CLAUDE.md as the load-bearing hub semantic.
	//
	// Negotiation-by-server is more appropriate for single-server
	// MCP endpoints; the hub's per-daemon aggregation can't safely
	// pick a version on the client's behalf because different
	// aggregated daemons may not agree.
	if !hubSupportedVersions[initParams.ProtocolVersion] {
		writeJSONRPCErrorStatus(w, reqID, http.StatusOK, -32600,
			"unsupported protocolVersion",
			map[string]any{
				"supported": hubSupportedVersionsList(),
				"requested": initParams.ProtocolVersion,
			})
		return
	}
	negotiatedVersion := initParams.ProtocolVersion

	// Capture the current resolver snapshot for the new session.
	snap := LoadResolverSnapshot()

	sess, err := h.sessions.Create(scopeKey, negotiatedVersion, snap)
	if err != nil {
		if errors.Is(err, ErrSessionCapExceeded) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSONRPCErrorStatus(w, reqID, http.StatusInternalServerError, -32603, "internal error: "+err.Error(), nil)
		return
	}
	sess.mu.Lock()
	sess.InstanceID = r.Header.Get("X-Mcphub-Instance-Id")
	sess.mu.Unlock()

	// Populate IntendedParticipants from the snapshot's bindings for
	// this scope key. AggregateInitialize fans out to every binding.
	// For a /clients/ request scopeKey is the bare client id (unchanged
	// behavior); for a /g/ request it is "g:<group>", so the SAME lookup
	// selects the group's member-server daemons (the tool-visibility
	// narrowing — groups/namespaces Phase 4b).
	if snap != nil {
		participants := make([]canonicalDaemonRef, len(snap.Bindings[scopeKey]))
		copy(participants, snap.Bindings[scopeKey])
		sess.IntendedParticipants = participants
	}

	respBody, aerr := AggregateInitialize(r.Context(), sess, reqID)
	if aerr != nil {
		// Roll back the session record so the failed initialize doesn't
		// leak a slot. Delete is idempotent; safe to call even if the
		// store eviction code is racing.
		h.sessions.Delete(sess.ClientSessionID)
		writeJSONRPCErrorStatus(w, reqID, http.StatusInternalServerError, -32603, "initialize fan-out: "+aerr.Error(), nil)
		return
	}

	// Hand the client session id back via the response header — the
	// client echoes this as Mcp-Session-Id on every subsequent call.
	w.Header().Set("Mcp-Session-Id", sess.ClientSessionID)
	writeRawJSON(w, respBody)
}

// handleDelete terminates a hub session + fans out best-effort
// DELETE /mcp to every daemon-side session in InitSuccesses. Always
// returns 204 on a known session (idempotent on a re-delete) and 404
// on an unknown one.
//
// Spec §"Client-origin lifecycle methods" — F-G4 fix.
//
// codex bot phase4 r1 P1 closure on PR #158: DELETE MUST enforce
// gate 7 (MCP-Protocol-Version) just like every other non-initialize
// method. The pre-r2 path validated token / instance / session /
// client binding but skipped the version header, so a valid session
// holder could terminate a session without negotiating the matching
// version. Same shape as handlePost lines 273-286.
func (h *HubMcpHandler) handleDelete(w http.ResponseWriter, r *http.Request, scopeKey string) {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sess, ok := h.sessions.Get(sid)
	if !ok {
		// Idempotent: a repeat DELETE after a successful one also
		// gets 404.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if sess.ScopeKey != scopeKey {
		// Cross-scope (and cross-kind) session reuse → 401 empty body.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Gate 7: MCP-Protocol-Version. Same contract as POST: missing
	// → 400 empty body; mismatched → 400 with a JSON-RPC error
	// envelope referencing the (null) id since DELETE has no request
	// body to echo.
	pv := r.Header.Get("MCP-Protocol-Version")
	if pv == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !hubSupportedVersions[pv] || pv != sess.ProtocolVersion {
		writeJSONRPCErrorStatus(w, json.RawMessage(`null`), http.StatusBadRequest, -32600, "protocol-version mismatch", nil)
		return
	}

	// Mark DELETE-started and snapshot init successes under the same session mu.
	// Detached reinit cache attempts use this lifecycle flag to avoid caching a
	// fresh daemon session after this DELETE's snapshot.
	daemonSessions := sess.markDeleteStartedAndSnapshotDaemonSessions()

	// codex bot phase4 r6 P1 closure on PR #158: invalidate the hub
	// session BEFORE the daemon fan-out. The fan-out can block up to
	// ~5s on per-daemon timeouts; during that window a concurrent
	// POST with the same Mcp-Session-Id would still pass gate 6 and
	// execute tools/call against an in-process-terminated session.
	// Deleting first gives the client immediate-revocation
	// semantics while preserving best-effort downstream cleanup.
	h.sessions.Delete(sid)

	// Best-effort fan-out: ignore errors. Even if every fan-out fails
	// we still return 204 — the client considers the session
	// terminated regardless of daemon-side state.
	//
	// codex bot phase4 r7 P2 closure on PR #158: derive per-iteration
	// ctx from context.Background() rather than r.Context(). A client
	// that disconnects right after sending DELETE would otherwise
	// cancel r.Context() immediately, short-circuiting every per-
	// daemon fan-out call before it attempts the DELETE — daemon-side
	// MCP sessions would then leak until their own idle-sweeper kicks
	// in. The 5-second budget remains; we just stop letting the
	// client steer it.
	//
	// codex bot phase4 r12 P2 closure on PR #158: give EACH daemon
	// its own 5 s budget rather than sharing one fanCtx across all
	// daemons. The shared budget would let a single slow/unreachable
	// daemon (a stuck `mcphub.exe daemon X` on its way to crashing)
	// consume the entire 5 s, causing every subsequent
	// bestEffortDeleteDaemonSession to short-circuit on its own
	// timeout-check without firing the DELETE — daemon-side sessions
	// on the FAST daemons would leak too. Per-iteration ctx makes
	// each participant's best-effort attempt independent.
	//
	// codex bot phase4 r18 P2 closure on PR #158: parallelize the
	// fan-out. Sequential per-daemon (r12) bounded each daemon's
	// share to 5 s but still made total DELETE latency = N × 5 s
	// in the worst case (every daemon unreachable). With 4 daemons
	// that's 20 s before the client gets its 204; clients would
	// time out and retry while the hub-side session was already
	// deleted. Goroutine-per-daemon under a single overall 5 s
	// deadline: total latency stays at ~5 s regardless of N, and a
	// single slow daemon doesn't serialize the others.
	if len(daemonSessions) > 0 {
		// Issue #159 concurrency lane #2 closure: replace
		// sync.WaitGroup + closer goroutine with a buffered
		// completion channel sized to the worker count. The
		// closer goroutine in the previous design leaked when
		// one worker wedged in a non-cancellable syscall —
		// wg.Wait() blocked forever inside it. With a buffered
		// chan sized exactly to len(daemonSessions), every
		// worker can ALWAYS write its completion sentinel and
		// exit cleanly without the supervising goroutine.
		// Workers that wedge inside http.Client.Do are still
		// long-lived, but the fan-out coordinator itself has no
		// extra goroutine that depends on every worker
		// finishing.
		done := make(chan struct{}, len(daemonSessions))
		for ref, state := range daemonSessions {
			go func(ref canonicalDaemonRef, state daemonInitState) {
				// defer signals completion to the buffered
				// chan; never blocks because cap == worker
				// count.
				defer func() { done <- struct{}{} }()
				fanCtx, fanCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer fanCancel()
				_ = bestEffortDeleteDaemonSession(fanCtx, ref, state.SessionID, state.ProtocolVersion)
			}(ref, state)
		}
		// Bound the total wait at 5 s + small slack. Workers
		// that finish in time tick the chan; the deadline path
		// stops waiting once 5500ms elapses.
		total := len(daemonSessions)
		deadline := time.After(5500 * time.Millisecond)
		got := 0
		for got < total {
			select {
			case <-done:
				got++
			case <-deadline:
				got = total // exit loop without closing/draining
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// bestEffortDeleteDaemonSession issues DELETE http://127.0.0.1:<port>/mcp
// with Mcp-Session-Id: <daemonSID>. Errors are returned to the caller
// but swallowed at the call site (the spec contract is "best-effort
// fan-out; 204 regardless").
func bestEffortDeleteDaemonSession(ctx context.Context, ref canonicalDaemonRef, daemonSID, protoVer string) error {
	u := clients.HubLoopbackURL(ref.Port, "/mcp")
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
	}
	if protoVer != "" {
		req.Header.Set("MCP-Protocol-Version", protoVer)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// hubScopeKind discriminates the two URL prefixes this handler serves.
// A client request keys on the bare client id; a group request keys on
// the kind-namespaced "g:<group>" scope key (groups/namespaces Phase 4b).
type hubScopeKind int

const (
	// kindClient is the /clients/<id>/mcp route. scopeKey = bare <id>.
	kindClient hubScopeKind = iota
	// kindGroup is the /g/<group>/mcp route. scopeKey = "g:"+<group>.
	kindGroup
)

// HubClientPrefix / HubGroupPrefix / HubPathSuffix are the URL grammar
// the hub mux routes on (mounted in internal/gui/hub_listener.go). Named
// constants so the parser and the listener mount reference the same
// literals. Exported so other packages (e.g. internal/gui/groups.go) build
// hub route URLs from this single owner instead of maintaining a mirrored
// copy of the same literals.
const (
	HubClientPrefix = "/clients/"
	HubGroupPrefix  = "/g/"
	HubPathSuffix   = "/mcp"
)

// parseHubPathFromURL extracts (kind, name) from the two hub request
// shapes:
//
//	/clients/<id>/mcp  → (kindClient, <id>)
//	/g/<group>/mcp      → (kindGroup,  <group>)
//
// Returns (_, "", false) on any other shape. The strict shape rules are
// IDENTICAL to the original client-only parser: the leading slash is
// mandatory, the trailing /mcp is mandatory, and the name segment must be
// non-empty with no embedded slash or whitespace (so /clients/foo/,
// /clients/foo/mcp/bar, /g/, and /g/a/b/mcp all reject). The /clients/
// branch is byte-equivalent to the pre-Phase-4b parseClientPathFromURL —
// every existing path that parsed before parses to the same (id) now.
//
// Group vs client disambiguation is purely by prefix; the kind-namespaced
// scope key (resolveHubScopeKey) keeps the two keyspaces disjoint so a
// group and a client of the same name can never collide downstream.
func parseHubPathFromURL(p string) (hubScopeKind, string, bool) {
	if name, ok := parseHubSegment(p, HubClientPrefix); ok {
		return kindClient, name, true
	}
	if name, ok := parseHubSegment(p, HubGroupPrefix); ok {
		return kindGroup, name, true
	}
	return kindClient, "", false
}

// parseHubSegment is the shared strict-shape extractor: given a prefix
// (e.g. "/clients/" or "/g/") it returns the single path segment between
// the prefix and the trailing "/mcp", rejecting any embedded slash /
// whitespace / emptiness. It is the one owner of the path grammar so the
// client and group routes can never drift apart.
func parseHubSegment(p, prefix string) (string, bool) {
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if !strings.HasSuffix(rest, HubPathSuffix) {
		return "", false
	}
	name := rest[:len(rest)-len(HubPathSuffix)]
	// Empty name, embedded slash, or whitespace → reject.
	if name == "" || strings.ContainsAny(name, "/ \t\r\n") {
		return "", false
	}
	return name, true
}

// parseClientPathFromURL extracts {client} from /clients/{client}/mcp.
// Returns ("", false) on any other shape. Retained as the byte-identical
// client-only parser the Phase 1/2 characterization + handler tests pin;
// it now delegates to the shared parseHubSegment so the two routes share
// one grammar owner.
//
// Spec §"Cross-client invariant" step 2: only `/clients/<id>/mcp`
// is accepted as a path; the strict shape closes off `/clients/foo/`
// (could collide with a future route) and `/clients/foo/mcp/bar`
// (drives the gate's lookup against an unrelated client id).
func parseClientPathFromURL(p string) (string, bool) {
	return parseHubSegment(p, HubClientPrefix)
}

// resolveHubScopeKey maps a parsed (kind, name) to the kind-namespaced
// scope key used by the token table, the session store, and the resolver
// snapshot — AND gates the name against the known-roster for its kind.
// Returns (scopeKey, known). A not-known result drives the gate-2 404.
//
//   - client → scopeKey = bare name, known iff isSupportedClient(name).
//     BYTE-IDENTICAL to the pre-Phase-4b gate.
//   - group  → scopeKey = "g:"+name, known iff isKnownGroup(name).
func resolveHubScopeKey(kind hubScopeKind, name string) (string, bool) {
	switch kind {
	case kindGroup:
		return GroupScopeKey(name), isKnownGroup(name)
	default:
		return name, isSupportedClient(name)
	}
}

// isSupportedClient returns true iff name is in
// clients.SupportedClientNames(). Centralizing the check here so a
// future expansion of the client roster requires only the one update
// in internal/clients/clients.go.
func isSupportedClient(name string) bool {
	for _, n := range clients.SupportedClientNames() {
		if n == name {
			return true
		}
	}
	return false
}

// isKnownGroup returns true iff a group named `name` is declared in the
// live hub state. The authoritative in-memory record is the published
// ResolverSnapshot's Groups set: BuildResolverSnapshotFromManifestsAndGroups
// records EVERY group in groups.yaml under its "g:<name>" scope key,
// regardless of whether its member servers currently resolve to live
// daemons. This makes a declared-but-empty group "known" (it passes gate 2,
// then routes nothing — the design-claim-5 degradation) rather than 404-ing,
// while an undeclared group is absent from the set and is rejected with the
// same empty-body 404 the unknown-client path uses.
//
// Sourcing the gate from the snapshot (the in-memory cache of groups.yaml,
// rebuilt + atomically republished on every group create/delete) — NOT the
// token table — keeps gate 2 (known) consistent with the config source of
// truth: a deleted group drops out of the next published snapshot and is
// immediately unknown, whereas a stale "g:<name>" token row left behind
// would otherwise keep a deleted group "known". The lookup is lock-free
// (LoadResolverSnapshot reads the atomic-pointer snapshot); a nil snapshot
// (no publish yet) yields a nil Groups map → not known.
func isKnownGroup(name string) bool {
	if validateGroupName(name) != nil {
		return false
	}
	snap := LoadResolverSnapshot()
	if snap == nil {
		return false
	}
	return snap.Groups[GroupScopeKey(name)]
}

// isLowerHex64 returns true iff s is exactly 64 lowercase hex chars.
// Matches the auth gate's "X-Mcphub-Hub-Token MUST be 64-lower-hex"
// rule (spec §"Cross-client invariant" step 3). Uppercase hex is
// rejected — the persisted tokens are emitted via hex.EncodeToString
// which is always lowercase, so an uppercase header on the wire
// indicates a manually-crafted (and thus suspicious) request.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// writeRawJSON writes body as-is with Content-Type: application/json
// + 200 OK. body is assumed to be a well-formed JSON-RPC envelope
// (the aggregator's response builders all return one). The handler
// uses this for tools/list and tools/call dispatch where the
// aggregator already produced the final bytes.
func writeRawJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	setResponseWriteDeadline(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func setResponseWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(hubMcpResponseWriteTimeout))
}

// writeJSONRPCResult emits {"jsonrpc":"2.0","id":<reqID>,"result":<result>}
// with HTTP 200. Used by ping (and any future hub-local method).
//
// codex bot phase4 r17 P2 closure on PR #158: a JSON-RPC notification
// (request without id) MUST NOT receive a response envelope per
// JSON-RPC 2.0 §4.1 + MCP §1.5. Detect missing id and emit HTTP 202
// with empty body instead of a synthetic id:null response — strict
// clients treating an unexpected response as a protocol error would
// otherwise drop subsequent valid responses on the same connection.
func writeJSONRPCResult(w http.ResponseWriter, reqID json.RawMessage, result any) {
	if isJSONRPCNotificationID(reqID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  result,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setResponseWriteDeadline(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// isJSONRPCNotificationID returns true ONLY when reqID is absent (the
// "id" field is missing from the JSON envelope entirely). A request
// MUST be treated as a notification — server MUST NOT reply.
//
// codex bot phase4 r19 P2 closure on PR #158: the pre-r19 variant
// also returned true for the literal `null` token, treating
// `"id": null` as a notification. That conflicts with MCP §1.5 which
// requires id to be a String or Number (non-null) — see
// newRequestIDKey in hub_mcp_request_id.go for the canonicalization
// rule. Now explicit `id:null` is classified as a real request that
// fails downstream validation with -32600 Invalid Request, surfacing
// the client bug instead of silently swallowing the response.
//
// Classification:
//   - len(reqID) == 0           : notification, no response (HTTP 202)
//   - reqID == "null"           : invalid request (rejected upstream)
//   - any other value           : real request, response required
func isJSONRPCNotificationID(reqID json.RawMessage) bool {
	return len(reqID) == 0
}

// writeJSONRPCErrorStatus emits a JSON-RPC error envelope with an
// explicit HTTP status code. data is optional (nil omits the field).
// id is echoed verbatim; an empty raw message renders as null.
//
// Used for spec-mandated error responses: -32700 parse error,
// -32600 invalid request / unsupported version / unknown session
// / protocol-version mismatch, -32601 method not found,
// -32603 internal error.
func writeJSONRPCErrorStatus(w http.ResponseWriter, reqID json.RawMessage, httpStatus, code int, message string, data any) {
	idField := reqID
	if len(idField) == 0 {
		idField = json.RawMessage(`null`)
	}
	errObj := map[string]any{
		"code":    code,
		"message": message,
	}
	if data != nil {
		errObj["data"] = data
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      idField,
		"error":   errObj,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	setResponseWriteDeadline(w)
	w.WriteHeader(httpStatus)
	_, _ = w.Write(payload)
}

// ----------------- Loopback guard (local copy) -----------------
//
// internal/daemon/loopback_guard.go already implements this logic for
// per-daemon HTTP listeners. internal/daemon imports internal/api
// (lazy_proxy.go), so we cannot import the other way without a cycle.
// The implementation here mirrors the daemon one byte-for-byte —
// keep both in sync if either evolves. (Spec §"Cross-client invariant"
// step 1 names the daemon function as the canonical reference.)

// isSafeLoopbackHubRequest returns true iff:
//   - Host is a loopback hostport (127.0.0.1, ::1, localhost), and
//   - Origin (if present) is also loopback, and
//   - Sec-Fetch-Site is one of "", "none", "same-origin", "same-site".
//
// Rejecting non-loopback Host defeats DNS-rebind; rejecting cross-site
// Sec-Fetch-Site defeats browser-driven CSRF via a token already
// present in another origin's cookie jar.
func isSafeLoopbackHubRequest(r *http.Request) bool {
	if !isLoopbackHostHub(r.Host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOriginHub(origin) {
		return false
	}
	return isSafeFetchSiteHub(r.Header.Get("Sec-Fetch-Site"))
}

func isLoopbackOriginHub(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return isLoopbackHostHub(u.Host)
}

func isSafeFetchSiteHub(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "same-origin", "same-site":
		return true
	default:
		return false
	}
}

func isLoopbackHostHub(hostport string) bool {
	host, ok := splitHostForLoopbackCheckHub(hostport)
	if !ok {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func splitHostForLoopbackCheckHub(hostport string) (string, bool) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" || strings.ContainsAny(hostport, "/@") {
		return "", false
	}
	if strings.HasPrefix(hostport, "[") {
		if strings.Contains(hostport, "]:") {
			host, port, err := net.SplitHostPort(hostport)
			if err != nil || !validHostPortHub(port) {
				return "", false
			}
			return host, true
		}
		if strings.HasSuffix(hostport, "]") {
			return strings.TrimPrefix(strings.TrimSuffix(hostport, "]"), "["), true
		}
		return "", false
	}
	if strings.Count(hostport, ":") == 1 {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil || !validHostPortHub(port) {
			return "", false
		}
		return host, true
	}
	return hostport, true
}

func validHostPortHub(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}
