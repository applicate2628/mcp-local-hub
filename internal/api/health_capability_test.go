package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capabilityProbeServer spins up an httptest server that answers the
// initialize handshake, then returns errBody verbatim for the list call.
// liveCapabilitySubSection dials http://127.0.0.1:<port>/mcp; the handler
// ignores the path so the httptest server (bound to 127.0.0.1) answers it.
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

// TestLiveCapabilitySubSection_MethodNotFoundNon32601 pins the G2 fix: a
// prompts/list error whose code is NOT -32601 but whose message says
// "Method not found" must classify as the neutral "unsupported" state, not
// the alarming red "error" state. This is the lldb-has-no-prompts case that
// rendered a scary red banner before the fix.
func TestLiveCapabilitySubSection_MethodNotFoundNon32601(t *testing.T) {
	cases := map[string]string{
		// Non-(-32601) code, classic "Method not found" message.
		"method-not-found": `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Method not found"}}`,
		// Mixed-case message — the match is case-insensitive.
		"mixed-case": `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"PROMPTS/LIST: Method Not Found"}}`,
		// "unsupported method" phrasing.
		"unsupported-method": `{"jsonrpc":"2.0","id":2,"error":{"code":1,"message":"unsupported method: prompts/list"}}`,
		// "unknown method" phrasing.
		"unknown-method": `{"jsonrpc":"2.0","id":2,"error":{"code":42,"message":"unknown method"}}`,
		// Bare "not found" phrasing.
		"not-found": `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"prompts not found"}}`,
	}
	for name, errBody := range cases {
		t.Run(name, func(t *testing.T) {
			port, closeSrv := capabilityProbeServer(t, errBody)
			defer closeSrv()

			a := &API{}
			got := a.liveCapabilitySubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "prompts/list", "prompt")
			if got.State != "unsupported" {
				t.Errorf("State = %q, want %q (err=%q)", got.State, "unsupported", got.Err)
			}
			if got.Err != "" {
				t.Errorf("Err should be empty for unsupported state, got %q", got.Err)
			}
		})
	}
}

// TestLiveCapabilitySubSection_Code32601StillUnsupported pins the existing
// strict-code path: a JSON-RPC error with code -32601 stays "unsupported".
func TestLiveCapabilitySubSection_Code32601StillUnsupported(t *testing.T) {
	port, closeSrv := capabilityProbeServer(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"whatever"}}`)
	defer closeSrv()

	a := &API{}
	got := a.liveCapabilitySubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "prompts/list", "prompt")
	if got.State != "unsupported" {
		t.Errorf("State = %q, want %q", got.State, "unsupported")
	}
}

// TestLiveCapabilitySubSection_GenuineErrorStillError pins that a genuine
// backend failure (not a method-absence message) still maps to the red
// "error" state — the fix must not over-broaden into swallowing real errors.
func TestLiveCapabilitySubSection_GenuineErrorStillError(t *testing.T) {
	port, closeSrv := capabilityProbeServer(t, `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"internal error"}}`)
	defer closeSrv()

	a := &API{}
	got := a.liveCapabilitySubSection(DaemonStatus{Server: "lldb", Daemon: "default", Port: port}, "prompts/list", "prompt")
	if got.State != "error" {
		t.Errorf("State = %q, want %q", got.State, "error")
	}
	if !strings.Contains(got.Err, "internal error") {
		t.Errorf("Err = %q, want it to contain %q", got.Err, "internal error")
	}
}
