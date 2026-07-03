package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capabilityProbeServer spins up an httptest server that answers the
// initialize handshake, then returns errBody verbatim for the list call.
// The post-initialize list probe (capabilityListSubSection) dials
// http://127.0.0.1:<port>/mcp; the handler ignores the path so the httptest
// server (bound to 127.0.0.1) answers it. Init declares an empty capabilities
// object — these tests drive capabilityListSubSection directly with a fixed
// "test-session", so the list-classification logic is exercised in isolation.
func capabilityProbeServer(t *testing.T, errBody string) (int, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		if strings.Contains(body, `"initialize"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		_, _ = w.Write([]byte(errBody))
	}))
	return parsePort(t, srv.URL), srv.Close
}

// TestCapabilityListSubSection_MethodNotFoundNon32601 pins the G2 backstop: a
// prompts/list error whose code is NOT -32601 but whose message says
// "Method not found" must classify as the neutral "unsupported" state, not
// the alarming red "error" state. This is the lldb-has-no-prompts case that
// rendered a scary red banner before the fix. Post-Phase-2 the G2 match is a
// non-conforming-server backstop on the probed path (the fallback path here
// probes because init declared empty capabilities).
func TestCapabilityListSubSection_MethodNotFoundNon32601(t *testing.T) {
	cases := map[string]string{
		// Non-(-32601) code, classic "Method not found" message.
		"method-not-found": `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Method not found"}}`,
		// Mixed-case message — the match is case-insensitive.
		"mixed-case": `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"PROMPTS/LIST: Method Not Found"}}`,
		// "unsupported method" phrasing.
		"unsupported-method": `{"jsonrpc":"2.0","id":2,"error":{"code":1,"message":"unsupported method: prompts/list"}}`,
		// "unknown method" phrasing.
		"unknown-method": `{"jsonrpc":"2.0","id":2,"error":{"code":42,"message":"unknown method"}}`,
	}
	for name, errBody := range cases {
		t.Run(name, func(t *testing.T) {
			port, closeSrv := capabilityProbeServer(t, errBody)
			defer closeSrv()

			a := &API{}
			got := a.capabilityListSubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "test-session", 2, "prompts/list", "prompt")
			if got.State != "unsupported" {
				t.Errorf("State = %q, want %q (err=%q)", got.State, "unsupported", got.Err)
			}
			if got.Err != "" {
				t.Errorf("Err should be empty for unsupported state, got %q", got.Err)
			}
		})
	}
}

// TestCapabilityListSubSection_NonMethodNotFoundStillError pins that backend
// failures containing "not found" are not hidden as unsupported capabilities
// unless the message specifically identifies method absence.
func TestCapabilityListSubSection_NonMethodNotFoundStillError(t *testing.T) {
	port, closeSrv := capabilityProbeServer(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"workspace not found: /tmp/acme-project"}}`)
	defer closeSrv()

	a := &API{}
	got := a.capabilityListSubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "test-session", 2, "tools/list", "tool")
	if got.State != "error" {
		t.Errorf("State = %q, want %q (err=%q)", got.State, "error", got.Err)
	}
	if !strings.Contains(got.Err, "workspace not found") {
		t.Errorf("Err = %q, want it to contain %q", got.Err, "workspace not found")
	}
}

// TestCapabilityListSubSection_Code32601StillUnsupported pins the existing
// strict-code path: a JSON-RPC error with code -32601 stays "unsupported".
func TestCapabilityListSubSection_Code32601StillUnsupported(t *testing.T) {
	port, closeSrv := capabilityProbeServer(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"whatever"}}`)
	defer closeSrv()

	a := &API{}
	got := a.capabilityListSubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "test-session", 2, "prompts/list", "prompt")
	if got.State != "unsupported" {
		t.Errorf("State = %q, want %q", got.State, "unsupported")
	}
}

// TestCapabilityListSubSection_GenuineErrorStillError pins that a genuine
// backend failure (not a method-absence message) still maps to the red
// "error" state — the fix must not over-broaden into swallowing real errors.
func TestCapabilityListSubSection_GenuineErrorStillError(t *testing.T) {
	port, closeSrv := capabilityProbeServer(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"internal error"}}`)
	defer closeSrv()

	a := &API{}
	got := a.capabilityListSubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "test-session", 2, "prompts/list", "prompt")
	if got.State != "error" {
		t.Errorf("State = %q, want %q", got.State, "error")
	}
	if !strings.Contains(got.Err, "internal error") {
		t.Errorf("Err = %q, want it to contain %q", got.Err, "internal error")
	}
}

// ---- Phase 2: realCapabilityRow declared-capabilities gating ----

// capabilityRowRecorder is a request-counting MCP stub that drives
// realCapabilityRow end to end. `capabilities` is the JSON emitted inside the
// initialize result's `capabilities` field (raw, so `absent` can omit the key
// entirely). listResults maps a method (e.g. "tools/list") to the verbatim
// list response body. Every request is counted and the Mcp-Session-Id header
// it carried is recorded, so a test can assert single-init + session reuse +
// the exact set of list calls that fired.
type capabilityRowRecorder struct {
	mu           sync.Mutex
	initCount    int
	listCounts   map[string]int    // method -> count
	sessionSeen  map[string]string // method -> Mcp-Session-Id sent on that list call
	idSeen       map[string]int    // method -> JSON-RPC id sent on that list call
	capabilities string            // raw JSON for result.capabilities, or "" to OMIT the key
	listResults  map[string]string // method -> verbatim list response
}

func newCapabilityRowServer(t *testing.T, rec *capabilityRowRecorder) (int, func()) {
	t.Helper()
	if rec.listCounts == nil {
		rec.listCounts = map[string]int{}
	}
	if rec.sessionSeen == nil {
		rec.sessionSeen = map[string]string{}
	}
	if rec.idSeen == nil {
		rec.idSeen = map[string]int{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-xyz")

		rec.mu.Lock()
		defer rec.mu.Unlock()
		if strings.Contains(body, `"initialize"`) {
			rec.initCount++
			capField := ""
			if rec.capabilities != "" {
				capField = `,"capabilities":` + rec.capabilities
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"%s}}`, capField)
			return
		}
		for _, method := range []string{"tools/list", "prompts/list", "resources/list"} {
			if strings.Contains(body, `"`+method+`"`) {
				rec.listCounts[method]++
				rec.sessionSeen[method] = r.Header.Get("Mcp-Session-Id")
				var idEnv struct {
					ID int `json:"id"`
				}
				_ = json.Unmarshal([]byte(body), &idEnv)
				rec.idSeen[method] = idEnv.ID
				out := rec.listResults[method]
				if out == "" {
					out = `{"jsonrpc":"2.0","id":2,"result":{}}` // empty list
				}
				_, _ = w.Write([]byte(out))
				return
			}
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	}))
	return parsePort(t, srv.URL), srv.Close
}

func (rec *capabilityRowRecorder) totalRequests() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	total := rec.initCount
	for _, c := range rec.listCounts {
		total += c
	}
	return total
}

// TestRealCapabilityRow_DeclaredPartial_SkipsUndeclaredProbes — a server
// declaring ONLY tools must get tools probed while prompts + resources are
// marked "unsupported" with ZERO list round-trips for them (the spec-correct
// path, the core Phase-2 win).
func TestRealCapabilityRow_DeclaredPartial_SkipsUndeclaredProbes(t *testing.T) {
	rec := &capabilityRowRecorder{
		capabilities: `{"tools":{}}`,
		listResults: map[string]string{
			"tools/list": `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"}]}}`,
		},
	}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	row, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port})
	if err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	if row.Tools.State != "ok" {
		t.Errorf("Tools.State = %q, want ok (declared + non-empty list)", row.Tools.State)
	}
	if row.Prompts.State != "unsupported" || row.Resources.State != "unsupported" {
		t.Errorf("undeclared categories: Prompts=%q Resources=%q, want unsupported/unsupported", row.Prompts.State, row.Resources.State)
	}
	if rec.listCounts["prompts/list"] != 0 || rec.listCounts["resources/list"] != 0 {
		t.Errorf("undeclared categories were PROBED: prompts=%d resources=%d, want 0/0", rec.listCounts["prompts/list"], rec.listCounts["resources/list"])
	}
	if rec.listCounts["tools/list"] != 1 {
		t.Errorf("tools/list count = %d, want 1", rec.listCounts["tools/list"])
	}
}

// TestRealCapabilityRow_DeclaredAll_ProbesAll — all three declared → all three
// probed.
func TestRealCapabilityRow_DeclaredAll_ProbesAll(t *testing.T) {
	rec := &capabilityRowRecorder{capabilities: `{"tools":{},"prompts":{},"resources":{}}`}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	if _, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port}); err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		if rec.listCounts[m] != 1 {
			t.Errorf("%s count = %d, want 1 (all declared → all probed)", m, rec.listCounts[m])
		}
	}
}

// TestRealCapabilityRow_EmptyCaps_FallbackProbesAll — a present-but-empty
// capabilities object must fall back to probing all three (never read {} as
// "everything unsupported"; this is the path the migrated G2 tests exercise).
func TestRealCapabilityRow_EmptyCaps_FallbackProbesAll(t *testing.T) {
	rec := &capabilityRowRecorder{capabilities: `{}`}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	if _, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port}); err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		if rec.listCounts[m] != 1 {
			t.Errorf("%s count = %d, want 1 (empty caps → probe-all fallback)", m, rec.listCounts[m])
		}
	}
}

// TestRealCapabilityRow_AbsentCaps_FallbackProbesAll — an initialize result
// with NO capabilities key at all must also fall back to probing all three.
func TestRealCapabilityRow_AbsentCaps_FallbackProbesAll(t *testing.T) {
	rec := &capabilityRowRecorder{capabilities: ""} // "" → the key is OMITTED
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	if _, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port}); err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		if rec.listCounts[m] != 1 {
			t.Errorf("%s count = %d, want 1 (absent caps → probe-all fallback)", m, rec.listCounts[m])
		}
	}
}

// TestRealCapabilityRow_SingleInitAndSessionReuse — exactly one initialize per
// daemon, and every list call carries the ONE session the init minted.
func TestRealCapabilityRow_SingleInitAndSessionReuse(t *testing.T) {
	rec := &capabilityRowRecorder{capabilities: `{"tools":{},"prompts":{},"resources":{}}`}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	if _, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port}); err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	if rec.initCount != 1 {
		t.Errorf("initialize count = %d, want exactly 1", rec.initCount)
	}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		if rec.sessionSeen[m] != "sess-xyz" {
			t.Errorf("%s carried Mcp-Session-Id %q, want the minted sess-xyz", m, rec.sessionSeen[m])
		}
	}
	// UNIQUE JSON-RPC ids within the one reused session (MCP spec: a request id
	// MUST NOT be reused within a session). initialize used id 1, so the three
	// lists must carry 2/3/4 — all distinct, none == 1.
	seen := map[int]string{1: "initialize"}
	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		id := rec.idSeen[m]
		if id == 0 {
			t.Errorf("%s carried no parseable JSON-RPC id", m)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s reused JSON-RPC id %d (already used by %s) — spec violation within the session", m, id, prev)
		}
		seen[id] = m
	}
}

// TestRealCapabilityRow_RoundTripCeiling — total requests never exceed 4
// (1 initialize + at most 3 lists), strictly below the pre-Phase-2 fixed 6.
func TestRealCapabilityRow_RoundTripCeiling(t *testing.T) {
	rec := &capabilityRowRecorder{capabilities: `{"tools":{},"prompts":{},"resources":{}}`}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	if _, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port}); err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	if got := rec.totalRequests(); got > 4 {
		t.Errorf("total round-trips = %d, want <= 4", got)
	}
}

// TestRealCapabilityRow_DeclaredButListErrors_MapsError — a DECLARED category
// whose list genuinely fails stays red "error", not "unsupported" (the state
// vocabulary must survive the refactor).
func TestRealCapabilityRow_DeclaredButListErrors_MapsError(t *testing.T) {
	rec := &capabilityRowRecorder{
		capabilities: `{"tools":{}}`,
		listResults: map[string]string{
			"tools/list": `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"internal boom"}}`,
		},
	}
	port, closeSrv := newCapabilityRowServer(t, rec)
	defer closeSrv()

	a := &API{}
	row, err := a.realCapabilityRow(DaemonStatus{Server: "srv", Daemon: "default", Port: port})
	if err != nil {
		t.Fatalf("realCapabilityRow: %v", err)
	}
	if row.Tools.State != "error" {
		t.Errorf("Tools.State = %q, want error (declared category, genuine list error)", row.Tools.State)
	}
	if !strings.Contains(row.Tools.Err, "internal boom") {
		t.Errorf("Tools.Err = %q, want it to contain the genuine error", row.Tools.Err)
	}
}
