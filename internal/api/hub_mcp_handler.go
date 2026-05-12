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
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
// "" if SetEndpoint has not yet been called. Used by gate 5 only.
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

	// Gate 2: path → canonical client_id.
	clientID, ok := parseClientPathFromURL(r.URL.Path)
	if !ok || !isSupportedClient(clientID) {
		// Unknown path or unknown client → 404 with EMPTY body so the
		// failure shape is identical regardless of whether the path is
		// well-formed-but-unknown vs malformed.
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// GET fallback (codex r7-bot-r5 P2 closure): runs gates 3-5 (token
	// + instance id) but is EXEMPT from gates 6-7 (no Mcp-Session-Id
	// or MCP-Protocol-Version requirement). MCP Streamable HTTP 2025-
	// 11-25 mandates 405 + `Allow: POST, DELETE` for GET on the endpoint.
	if r.Method == http.MethodGet {
		if !h.checkTokenAndInstanceID(w, r, clientID) {
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Gates 3-5: token shape + constant-time compare + instance id.
	if !h.checkTokenAndInstanceID(w, r, clientID) {
		return
	}

	// Gates 6-7 + dispatch live in per-method handlers.
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r, clientID)
	case http.MethodDelete:
		h.handleDelete(w, r, clientID)
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
func (h *HubMcpHandler) checkTokenAndInstanceID(w http.ResponseWriter, r *http.Request, clientID string) bool {
	tok := r.Header.Get("X-Mcphub-Hub-Token")
	// Gate 3: shape (64 lower-hex).
	if !isLowerHex64(tok) {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	// Gate 4: constant-time compare. Returns 1 only when the table has
	// a 64-hex entry for clientID AND it matches tok byte-for-byte.
	if ConstantTimeCompareToken(clientID, tok) != 1 {
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
// dispatches the JSON-RPC method via aggregator entry points.
func (h *HubMcpHandler) handlePost(w http.ResponseWriter, r *http.Request, clientID string) {
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
		h.handleInitialize(w, r, clientID, env.ID, env.Params)
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
	// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1):
	// GetAndTouch is one atomic store-lock acquisition that fetches
	// + refreshes LastUsedAt. The pre-r24 sequence of Get (RLock)
	// then Touch (Lock) had a sweep-Delete race window; the handler
	// could continue with a stale *hubSession after the sweep
	// removed the map entry, causing later cancellation/DELETE on
	// the same id to see "unknown session" mid-flight.
	sess, ok := h.sessions.GetAndTouch(sid)
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
	if sess.Client != clientID {
		// Cross-client session reuse → 401 empty body (no oracle).
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

	// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1):
	// activity timestamp already refreshed by GetAndTouch above.
	// Touch call removed — was the racing surface that prompted
	// the GetAndTouch atomic refactor.

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
		respBody, aerr := AggregateToolsList(r.Context(), sess, env.ID)
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
// session is created (codex r7-bot-r2 P2 closure).
func (h *HubMcpHandler) handleInitialize(w http.ResponseWriter, r *http.Request, clientID string, reqID json.RawMessage, paramsRaw json.RawMessage) {
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
	// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #2):
	// MCP §1.6: if the server doesn't support the offered version,
	// it MUST respond with another supported version (i.e.
	// negotiate). The pre-r24 path rejected with -32600 instead of
	// negotiating. Now the server picks its preferred supported
	// version and creates the session with that version. The
	// client decides whether to continue or disconnect based on
	// the InitializeResult.protocolVersion echo.
	negotiatedVersion := initParams.ProtocolVersion
	if !hubSupportedVersions[negotiatedVersion] {
		negotiatedVersion = "2025-11-25"
	}

	// Capture the current resolver snapshot for the new session.
	snap := LoadResolverSnapshot()

	sess, err := h.sessions.Create(clientID, negotiatedVersion, snap)
	if err != nil {
		if errors.Is(err, ErrSessionCapExceeded) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSONRPCErrorStatus(w, reqID, http.StatusInternalServerError, -32603, "internal error: "+err.Error(), nil)
		return
	}

	// Populate IntendedParticipants from the snapshot's bindings for
	// this client. AggregateInitialize fans out to every binding.
	if snap != nil {
		participants := make([]canonicalDaemonRef, len(snap.Bindings[clientID]))
		copy(participants, snap.Bindings[clientID])
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
func (h *HubMcpHandler) handleDelete(w http.ResponseWriter, r *http.Request, clientID string) {
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
	if sess.Client != clientID {
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

	// Snapshot init successes under the session mu so we don't race
	// with a concurrent tools/list that may be mutating the map.
	sess.mu.Lock()
	daemonSessions := make(map[canonicalDaemonRef]string, len(sess.InitSuccesses))
	for ref, dsid := range sess.InitSuccesses {
		daemonSessions[ref] = dsid
	}
	sess.mu.Unlock()

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
		var wg sync.WaitGroup
		for ref, dsid := range daemonSessions {
			wg.Add(1)
			go func(ref canonicalDaemonRef, dsid string) {
				defer wg.Done()
				fanCtx, fanCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer fanCancel()
				_ = bestEffortDeleteDaemonSession(fanCtx, ref, dsid)
			}(ref, dsid)
		}
		// Bound the total wait at 5 s + small slack so the HTTP
		// response cannot block indefinitely even if a daemon DELETE
		// goroutine is wedged in a non-cancellable syscall. The
		// individual goroutines outlive the wait if needed —
		// best-effort fan-out is fire-and-forget on the slow path.
		waitDone := make(chan struct{})
		go func() { wg.Wait(); close(waitDone) }()
		select {
		case <-waitDone:
		case <-time.After(5500 * time.Millisecond):
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// bestEffortDeleteDaemonSession issues DELETE http://127.0.0.1:<port>/mcp
// with Mcp-Session-Id: <daemonSID>. Errors are returned to the caller
// but swallowed at the call site (the spec contract is "best-effort
// fan-out; 204 regardless").
func bestEffortDeleteDaemonSession(ctx context.Context, ref canonicalDaemonRef, daemonSID string) error {
	u := fmt.Sprintf("http://127.0.0.1:%d/mcp", ref.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	if daemonSID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSID)
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

// parseClientPathFromURL extracts {client} from /clients/{client}/mcp.
// Returns ("", false) on any other shape. The leading slash is
// mandatory; trailing slashes and additional segments are rejected.
//
// Spec §"Cross-client invariant" step 2: only `/clients/<id>/mcp`
// is accepted as a path; the strict shape closes off `/clients/foo/`
// (could collide with a future route) and `/clients/foo/mcp/bar`
// (drives the gate's lookup against an unrelated client id).
func parseClientPathFromURL(p string) (string, bool) {
	const prefix = "/clients/"
	const suffix = "/mcp"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	id := rest[:len(rest)-len(suffix)]
	// Empty id, embedded slash, or whitespace → reject.
	if id == "" || strings.ContainsAny(id, "/ \t\r\n") {
		return "", false
	}
	return id, true
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
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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

// isJSONRPCNullID returns true when the request id is the literal
// JSON `null` token. MCP §1.5 requires id to be a non-null
// String/Number; the handler rejects such envelopes with -32600
// before reaching method dispatch. Helper extracted so the rule is
// auditable alongside isJSONRPCNotificationID.
func isJSONRPCNullID(reqID json.RawMessage) bool {
	if len(reqID) == 0 {
		return false
	}
	return string(bytes.TrimSpace(reqID)) == "null"
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
