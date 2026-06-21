// hub_mcp_handler_test.go — Phase 4 Task 4.2 (G4 unified hub MCP).
//
// Exercises the 7-check auth gate + JSON-RPC dispatch in
// hub_mcp_handler.go. Tests are intentionally scoped per gate
// (loopback / path / token-shape / constant-time / instance-id /
// session-client / mcp-protocol-version) so a single regression
// surfaces in a single failing test.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Cross-client invariant — seven-check auth gate".
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeToken is a 64-lower-hex token used by tests that need a
// shape-valid value. Distinct from realToken so we can verify
// shape-passes-but-compare-fails paths.
const fakeToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// realToken is a 64-lower-hex token published into liveTokenTable
// before each test via publishTestTokenTable. Distinct from fakeToken
// so token-shape passes but the constant-time compare can reject.
const realToken = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdef0123"

// realInstanceID is the 64-hex InstanceID handlers compare against via
// gate 5. Tests that need a positive instance-id match send this value;
// tests that exercise a mismatch send fakeInstanceID.
const realInstanceID = "1111111122222222333333334444444455555555666666667777777788888888"
const fakeInstanceID = "9999999999999999999999999999999999999999999999999999999999999999"

type deadlineRecorder struct {
	header              http.Header
	status              int
	body                bytes.Buffer
	deadline            time.Time
	writeBeforeDeadline bool
}

func (r *deadlineRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *deadlineRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
}

func (r *deadlineRecorder) Write(p []byte) (int, error) {
	if r.deadline.IsZero() {
		r.writeBeforeDeadline = true
	}
	return r.body.Write(p)
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadline = deadline
	return nil
}

// publishTestTokenTable swaps the live token table to a fresh map
// with one entry for "claude-code" and "codex-cli". Callers MUST
// invoke this in every test that exercises gate 4 — the global
// atomic.Pointer otherwise persists across tests.
func publishTestTokenTable(t *testing.T) {
	t.Helper()
	publishTokenTable(HubTokenTable{Tokens: map[string]string{
		"claude-code": realToken,
		"codex-cli":   realToken,
	}})
}

// newTestHandler returns a HubMcpHandler with a fresh sessions store
// + endpoint snapshot. Caller closes the store via t.Cleanup.
func newTestHandler(t *testing.T) *HubMcpHandler {
	t.Helper()
	publishTestTokenTable(t)
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient:  2,
		MaxGlobal:     8,
		IdleTimeout:   60 * 1000_000_000, // 60s — long enough for the test runtime
		SweepInterval: 60 * 1000_000_000,
	})
	t.Cleanup(store.Close)
	h := NewHubMcpHandler(store)
	h.SetEndpoint(HubEndpoint{InstanceID: realInstanceID, Port: 9120})
	return h
}

// authedRequest builds a request carrying every gate-passing header
// for `claude-code`. Callers tweak the returned request before
// passing to h.ServeHTTP.
func authedRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1:9120"
	req.Header.Set("X-Mcphub-Hub-Token", realToken)
	req.Header.Set("X-Mcphub-Instance-Id", realInstanceID)
	return req
}

func TestWriteRawJSONSetsWriteDeadlineBeforeBodyWrite(t *testing.T) {
	rec := &deadlineRecorder{}
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	start := time.Now()

	writeRawJSON(rec, payload)

	if rec.status != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.status)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", rec.Header().Get("Content-Type"))
	}
	if rec.writeBeforeDeadline {
		t.Fatalf("response body was written before SetWriteDeadline")
	}
	if rec.deadline.IsZero() {
		t.Fatalf("SetWriteDeadline was not called")
	}
	if !rec.deadline.After(start) || rec.deadline.After(start.Add(hubMcpResponseWriteTimeout+2*time.Second)) {
		t.Fatalf("deadline=%v want within response write budget from %v", rec.deadline, start)
	}
	if rec.body.String() != string(payload) {
		t.Fatalf("body=%s want %s", rec.body.String(), payload)
	}
}

// TestHandlerLoopbackGuardRejectsNonLoopbackHost — gate 1: Host
// outside the loopback set returns 403.
func TestHandlerLoopbackGuardRejectsNonLoopbackHost(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Host = "evil.example.com"
	req.Header.Set("X-Mcphub-Hub-Token", realToken)
	req.Header.Set("X-Mcphub-Instance-Id", realInstanceID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host: got %d, want 403", w.Code)
	}
}

// TestHandlerLoopbackGuardRejectsCrossSiteFetch — gate 1: a
// browser-emitted Sec-Fetch-Site of "cross-site" returns 403 even
// from a loopback Host (defeats DNS-rebind via browser fetch).
func TestHandlerLoopbackGuardRejectsCrossSiteFetch(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site fetch: got %d, want 403", w.Code)
	}
}

// TestHandlerUnknownPathReturns404EmptyBody — gate 2: a path that
// doesn't match /clients/{id}/mcp returns 404 with empty body.
func TestHandlerUnknownPathReturns404EmptyBody(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d, want 404", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("unknown path: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerUnknownClientReturns404EmptyBody — gate 2: a path that
// matches /clients/{id}/mcp but with an unknown client id returns
// 404 with empty body (identical shape to unknown-path case so no
// oracle).
func TestHandlerUnknownClientReturns404EmptyBody(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/not-a-client/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown client: got %d, want 404", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("unknown client: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerTokenShapeRejects63Hex — gate 3: a 63-char token (one
// byte short) returns 401 with empty body. The "63 lower-hex chars"
// case is the most common shape mistake an attacker would try
// (truncated token harvest).
func TestHandlerTokenShapeRejects63Hex(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Header.Set("X-Mcphub-Hub-Token", realToken[:63])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("63-hex token: got %d, want 401", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("63-hex token: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerTokenShapeRejectsUppercaseHex — gate 3: uppercase hex
// is rejected. Persisted tokens are emitted lowercase; uppercase
// indicates a manually-crafted header.
func TestHandlerTokenShapeRejectsUppercaseHex(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Header.Set("X-Mcphub-Hub-Token", strings.ToUpper(realToken))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("uppercase token: got %d, want 401", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("uppercase token: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerWrongTokenReturns401Constant — gate 4: a shape-valid
// but wrong-value token returns the SAME empty 401 as the 63-hex
// case. Asserts no oracle exists between gate 3 and gate 4.
func TestHandlerWrongTokenReturns401Constant(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Header.Set("X-Mcphub-Hub-Token", fakeToken) // shape-valid, wrong value
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("wrong token: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerWrongInstanceIDReturns401 — gate 5: a valid token but
// mismatched X-Mcphub-Instance-Id returns 401 with empty body.
func TestHandlerWrongInstanceIDReturns401(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", nil)
	req.Header.Set("X-Mcphub-Instance-Id", fakeInstanceID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong instance id: got %d, want 401", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("wrong instance id: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerMissingSessionIDOnNonInitializeReturns400 — gate 6: a
// non-initialize POST without Mcp-Session-Id returns 400 with empty
// body. Codex r7-bot-r2 P2 closure (earlier wording made the check
// conditional on header presence).
func TestHandlerMissingSessionIDOnNonInitializeReturns400(t *testing.T) {
	h := newTestHandler(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing session id: got %d, want 400", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("missing session id: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerCrossClientSessionReuseReturns401 — gate 6: a session
// created via claude-code, used on the codex-cli path with a
// codex-cli token, returns 401 (no cross-client tool-call leakage).
func TestHandlerCrossClientSessionReuseReturns401(t *testing.T) {
	h := newTestHandler(t)
	// Manually mint a claude-code session in the store.
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/codex-cli/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-client session reuse: got %d, want 401", w.Code)
	}
}

// TestHandlerProtocolVersionMissingReturns400 — gate 7: a session-
// bearing POST with no MCP-Protocol-Version header returns 400 with
// empty body (codex r7-bot-r6 P2 closure).
func TestHandlerProtocolVersionMissingReturns400(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	// No MCP-Protocol-Version header.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing version: got %d, want 400", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("missing version: body must be empty; got %q", w.Body.String())
	}
}

// TestHandlerProtocolVersionMismatchReturns400Minus32600 — gate 7:
// version header mismatched against the session's negotiated value
// returns 400 + JSON-RPC body with code=-32600.
func TestHandlerProtocolVersionMismatchReturns400Minus32600(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-06-18") // valid but mismatch with session 2025-11-25
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("version mismatch: got %d, want 400", w.Code)
	}
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("version mismatch: body parse: %v / body=%q", err, w.Body.String())
	}
	if env.Error.Code != -32600 {
		t.Errorf("version mismatch: got code %d, want -32600", env.Error.Code)
	}
}

func TestHandlerToolsListMetaUsesCachedInstanceIDWhenEndpointFileUnreadable(t *testing.T) {
	restore := SetDaemonStateRootForTest(t.TempDir())
	t.Cleanup(restore)

	d1 := newStubDaemon(t, "d1-sid")
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	ref := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port}
	sess.IntendedParticipants = []canonicalDaemonRef{ref}
	sess.InitSuccesses[ref] = "d1-sid"
	sess.DaemonProtoVer[ref] = "2025-11-25"

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d want 200; body=%s", w.Code, w.Body.String())
	}

	var env struct {
		Result struct {
			Meta struct {
				Mcphub struct {
					InstanceID string `json:"instance_id"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse tools/list response: %v body=%s", err, w.Body.String())
	}
	if env.Result.Meta.Mcphub.InstanceID != realInstanceID {
		t.Fatalf("tools/list _meta.mcphub.instance_id=%q want cached %q", env.Result.Meta.Mcphub.InstanceID, realInstanceID)
	}
}

// TestHandlerInitializeUnsupportedVersionReturnsSyncJSONRPCError —
// codex r7-bot-r2 P2 closure: an `initialize` with an unsupported
// protocolVersion returns a JSON-RPC error envelope synchronously
// and does NOT allocate a session.
func TestHandlerInitializeUnsupportedVersionReturnsSyncJSONRPCError(t *testing.T) {
	h := newTestHandler(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1900-01-01"}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		// The spec lets the version-rejection use HTTP 200 because the
		// envelope itself encodes the failure. Earlier drafts used 400;
		// the test pins our chosen status to OK so the error survives
		// the client-side json-rpc dispatcher (which routes purely on
		// envelope.error).
		t.Errorf("unsupported version: got HTTP %d, want 200", w.Code)
	}
	var env struct {
		Error struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unsupported version: body parse: %v / body=%q", err, w.Body.String())
	}
	if env.Error.Code != -32600 {
		t.Errorf("unsupported version: got code %d, want -32600", env.Error.Code)
	}
	if env.Error.Data == nil {
		t.Errorf("unsupported version: data field must enumerate supported versions")
	}
	// Assert no session was created.
	if got := len(h.sessions.sessions); got != 0 {
		t.Errorf("unsupported version: session count %d, want 0 (no allocation on rejection)", got)
	}
}

// TestHandlerGETReturns405WithAllowHeader — codex r7-bot-r5 P2:
// GET on /clients/{id}/mcp returns 405 with `Allow: POST, DELETE`.
// Gates 1-5 still run; an unauthed GET returns 401 (verified in a
// separate test) but an authed GET without Mcp-Session-Id gets 405
// (no 400 surprise from gate 6, which is exempted for GET).
func TestHandlerGETReturns405WithAllowHeader(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodGet, "/clients/claude-code/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST, DELETE" {
		t.Errorf("GET: Allow header = %q, want %q", allow, "POST, DELETE")
	}
}

// TestHandlerGETUnsupportedProtocolVersionReturns400 — issue #159
// protocol lane #4 closure: an EXPLICIT invalid MCP-Protocol-Version
// on a GET request must return 400 with the supported list, not 405.
// The MCP Streamable HTTP spec orders version validation BEFORE
// method-not-allowed. An empty/absent header still returns 405
// (no implied version) — covered by TestHandlerGETReturns405WithAllowHeader.
func TestHandlerGETUnsupportedProtocolVersionReturns400(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodGet, "/clients/claude-code/mcp", nil)
	req.Header.Set("MCP-Protocol-Version", "1900-01-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET with unsupported version: got %d, want 400 (version validation precedes 405)", w.Code)
	}
	var body struct {
		Error     string   `json:"error"`
		Requested string   `json:"requested"`
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body parse: %v / body=%q", err, w.Body.String())
	}
	if !strings.Contains(body.Error, "unsupported") {
		t.Errorf("error message = %q, want substring 'unsupported'", body.Error)
	}
	if body.Requested != "1900-01-01" {
		t.Errorf("requested = %q, want 1900-01-01", body.Requested)
	}
	if len(body.Supported) == 0 {
		t.Errorf("supported list must enumerate hub-supported versions; got empty")
	}
}

// TestHandlerGETWithoutAuthReturns401 — codex r7-bot-r5 P2: gates
// 1-5 still run for GET. An unauthed GET gets 401, not 405.
func TestHandlerGETWithoutAuthReturns401(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/clients/claude-code/mcp", nil)
	req.Host = "127.0.0.1:9120"
	// No token.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthed GET: got %d, want 401", w.Code)
	}
}

// TestHandlerDELETETerminatesSessionReturns204 — F-G4 fix: DELETE
// /clients/{id}/mcp with a known Mcp-Session-Id returns 204 + removes
// the session from the store. codex bot phase4 r1 P2 / codex deep-sec
// P1 closure on PR #158: MCP-Protocol-Version header is mandatory.
func TestHandlerDELETETerminatesSessionReturns204(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", w.Code)
	}
	if _, ok := h.sessions.Get(sess.ClientSessionID); ok {
		t.Errorf("DELETE: session still present in store after termination")
	}
}

// TestHandlerDELETERequiresProtocolVersionHeader — codex bot phase4
// r1 P2 / codex deep-sec P1 closure on PR #158. DELETE must enforce
// gate 7 (MCP-Protocol-Version) just like every other non-initialize
// method. Missing header → 400 empty body. Mismatched version → 400
// with JSON-RPC error envelope referencing null id.
func TestHandlerDELETERequiresProtocolVersionHeader(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Case 1: missing header → 400, session preserved (we did not
	// even reach the terminate logic).
	req := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	// Intentionally omit MCP-Protocol-Version.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing header DELETE: got %d, want 400", w.Code)
	}
	if _, ok := h.sessions.Get(sess.ClientSessionID); !ok {
		t.Errorf("missing-header DELETE removed the session; gate 7 should reject before terminating")
	}

	// Case 2: mismatched version → 400 with JSON-RPC error body,
	// session still preserved.
	req2 := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
	req2.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req2.Header.Set("MCP-Protocol-Version", "2024-11-05") // session is 2025-11-25
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("mismatch DELETE: got %d, want 400", w2.Code)
	}
	if _, ok := h.sessions.Get(sess.ClientSessionID); !ok {
		t.Errorf("mismatch DELETE removed the session; gate 7 should reject before terminating")
	}
}

// TestHandlerDELETEIdempotentOnSecondCall — a repeat DELETE after a
// successful one returns 404 (idempotent: client can retry without
// observing a different outcome that would imply a different state).
func TestHandlerDELETEIdempotentOnSecondCall(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// First DELETE succeeds.
	req1 := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
	req1.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req1.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)
	if w1.Code != http.StatusNoContent {
		t.Fatalf("first DELETE: got %d, want 204", w1.Code)
	}
	// Second DELETE returns 404 (session no longer present).
	req2 := authedRequest(t, http.MethodDelete, "/clients/claude-code/mcp", nil)
	req2.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("second DELETE: got %d, want 404", w2.Code)
	}
}

// TestHandlerInitializeRejectsExistingSessionID — initialize with
// an Mcp-Session-Id header set is a protocol violation (the client
// should be doing a fresh negotiation). Returns 400 with -32600.
func TestHandlerInitializeRejectsExistingSessionID(t *testing.T) {
	h := newTestHandler(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", "abc")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("initialize with session id: got %d, want 400", w.Code)
	}
}

// TestHandlerPingEchoes — hub-local ping returns {"result":{}} +
// id passes through verbatim (number vs string preserved).
func TestHandlerPingEchoes(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":"client-42","method":"ping"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("ping: got %d, want 200", w.Code)
	}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]any  `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("ping body parse: %v / body=%q", err, w.Body.String())
	}
	if string(env.ID) != `"client-42"` {
		t.Errorf("ping id: got %s, want \"client-42\" (string preservation)", env.ID)
	}
}

func TestHandlerToolsListUsesSessionCachedInstanceID(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	d1 := newStubDaemon(t, "d1-sid")
	ref := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port}
	PublishResolverSnapshot(&ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {ref}},
	})

	h := newTestHandler(t)
	initBody := []byte(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	initReq := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", initBody)
	initW := httptest.NewRecorder()
	h.ServeHTTP(initW, initReq)
	if initW.Code != http.StatusOK {
		t.Fatalf("initialize: got %d, want 200; body=%s", initW.Code, initW.Body.String())
	}
	sid := initW.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not return Mcp-Session-Id")
	}

	h.SetEndpoint(HubEndpoint{InstanceID: fakeInstanceID, Port: 9120})
	listBody := []byte(`{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`)
	listReq := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", listBody)
	listReq.Header.Set("X-Mcphub-Instance-Id", fakeInstanceID)
	listReq.Header.Set("Mcp-Session-Id", sid)
	listReq.Header.Set("MCP-Protocol-Version", "2025-11-25")
	listW := httptest.NewRecorder()
	h.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("tools/list: got %d, want 200; body=%s", listW.Code, listW.Body.String())
	}

	var env struct {
		Result struct {
			Meta struct {
				Mcphub struct {
					InstanceID string `json:"instance_id"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse tools/list body: %v / body=%s", err, listW.Body.String())
	}
	if env.Result.Meta.Mcphub.InstanceID != realInstanceID {
		t.Fatalf("tools/list instance_id=%q want initialized session instance_id %q", env.Result.Meta.Mcphub.InstanceID, realInstanceID)
	}
}

// TestHandlerInvalidJSONRPCEnvelopeReturns400 — a body missing the
// "jsonrpc":"2.0" field returns 400 with -32600.
func TestHandlerInvalidJSONRPCEnvelopeReturns400(t *testing.T) {
	h := newTestHandler(t)
	body := []byte(`{"id":1,"method":"tools/list"}`) // no jsonrpc field
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid envelope: got %d, want 400", w.Code)
	}
}

// TestHandlerMalformedJSONReturns400 — a body that fails JSON parse
// returns 400 with -32700 parse error.
func TestHandlerMalformedJSONReturns400(t *testing.T) {
	h := newTestHandler(t)
	body := []byte(`{not-json`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed json: got %d, want 400", w.Code)
	}
}

// TestHandlerUnsupportedMethodReturnsMinus32601 — a JSON-RPC method
// the hub does not implement returns -32601 (HTTP 200 — the envelope
// carries the error code).
func TestHandlerUnsupportedMethodReturnsMinus32601(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("unsupported method: got %d, want 200", w.Code)
	}
	var env struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if env.Error.Code != -32601 {
		t.Errorf("unsupported method: got code %d, want -32601", env.Error.Code)
	}
}

// TestHandlerPUTReturns405 — any method other than GET/POST/DELETE
// returns 405 with `Allow: POST, DELETE`.
func TestHandlerPUTReturns405(t *testing.T) {
	h := newTestHandler(t)
	req := authedRequest(t, http.MethodPut, "/clients/claude-code/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: got %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST, DELETE" {
		t.Errorf("PUT: Allow header = %q, want %q", allow, "POST, DELETE")
	}
}

// TestHandlerNotificationsCancelledForwardsAndRemoves — gate 7-passing
// notifications/cancelled returns 202 + (in production) forwards to
// the daemon. In this test we only verify the 202 status — full fan-
// out forwarding is covered by the aggregator's own tests.
func TestHandlerNotificationsCancelledForwardsAndRemoves(t *testing.T) {
	h := newTestHandler(t)
	sess, err := h.sessions.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":42}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", body)
	req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("notifications/cancelled: got %d, want 202", w.Code)
	}
}

// TestHandlerAuthGateMatrix exercises every (method, client, token,
// instance-id) combination at a quick scan-resolution. The bot keeps
// catching regressions where a P2 path-shape bug got introduced by
// a refactor; the matrix is the cheapest catch-all guard.
func TestHandlerAuthGateMatrix(t *testing.T) {
	h := newTestHandler(t)
	tcs := []struct {
		name      string
		method    string
		path      string
		token     string
		instance  string
		wantHTTP  int
		wantEmpty bool // body must be empty when wantHTTP is 401/403/405-pre-gate-7
	}{
		{name: "POST claude-code wrong-token", method: http.MethodPost, path: "/clients/claude-code/mcp", token: fakeToken, instance: realInstanceID, wantHTTP: http.StatusUnauthorized, wantEmpty: true},
		{name: "POST claude-code wrong-instance", method: http.MethodPost, path: "/clients/claude-code/mcp", token: realToken, instance: fakeInstanceID, wantHTTP: http.StatusUnauthorized, wantEmpty: true},
		{name: "POST claude-code short-token", method: http.MethodPost, path: "/clients/claude-code/mcp", token: realToken[:32], instance: realInstanceID, wantHTTP: http.StatusUnauthorized, wantEmpty: true},
		{name: "POST unknown-client right-token", method: http.MethodPost, path: "/clients/zoom/mcp", token: realToken, instance: realInstanceID, wantHTTP: http.StatusNotFound, wantEmpty: true},
		{name: "DELETE claude-code wrong-token", method: http.MethodDelete, path: "/clients/claude-code/mcp", token: fakeToken, instance: realInstanceID, wantHTTP: http.StatusUnauthorized, wantEmpty: true},
		{name: "GET claude-code wrong-token", method: http.MethodGet, path: "/clients/claude-code/mcp", token: fakeToken, instance: realInstanceID, wantHTTP: http.StatusUnauthorized, wantEmpty: true},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = "127.0.0.1:9120"
			req.Header.Set("X-Mcphub-Hub-Token", tc.token)
			req.Header.Set("X-Mcphub-Instance-Id", tc.instance)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.wantHTTP {
				t.Errorf("%s: got %d, want %d", tc.name, w.Code, tc.wantHTTP)
			}
			if tc.wantEmpty && w.Body.Len() != 0 {
				t.Errorf("%s: body must be empty; got %q", tc.name, w.Body.String())
			}
		})
	}
}

// TestParseClientPathFromURL exercises the path-parser unit directly.
// Several adversarial shapes need to be rejected; this is the
// cheapest place to verify them.
func TestParseClientPathFromURL(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"/clients/claude-code/mcp", "claude-code", true},
		{"/clients/codex-cli/mcp", "codex-cli", true},
		{"/clients//mcp", "", false},
		{"/clients/x", "", false},
		{"/clients/x/", "", false},
		{"/clients/x/mcp/extra", "", false},
		{"/clients/x with space/mcp", "", false},
		{"/clients/", "", false},
		{"/", "", false},
		{"", "", false},
		{"/clients/x%20y/mcp", "x%20y", true}, // url.Parse doesn't decode here; raw kept
	}
	for _, c := range cases {
		got, ok := parseClientPathFromURL(c.in)
		if got != c.wantID || ok != c.wantOK {
			t.Errorf("parseClientPathFromURL(%q) = (%q, %v); want (%q, %v)", c.in, got, ok, c.wantID, c.wantOK)
		}
	}
}

// TestIsLowerHex64 sanity-checks the shape gate. Lowercase 64-hex
// passes; everything else fails.
func TestIsLowerHex64(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{realToken, true},
		{strings.ToUpper(realToken), false},
		{realToken[:63], false},
		{realToken + "0", false},
		{strings.Repeat("g", 64), false}, // invalid hex char
		{"", false},
	}
	for _, c := range cases {
		if got := isLowerHex64(c.in); got != c.want {
			t.Errorf("isLowerHex64(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
