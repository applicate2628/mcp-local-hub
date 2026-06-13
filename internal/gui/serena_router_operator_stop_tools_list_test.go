// internal/gui/serena_router_operator_stop_tools_list_test.go
//
// r37-2 P2 regression: the serena tools/list flow must EXCLUDE a candidate
// whose wake was refused with api.ErrWakeRefusedOperatorStop (an operator
// deliberately stopped that serena daemon) from the fetch loop, mirroring the
// tool-call path's terminal handling (serena_router.go). Otherwise the
// operator-stopped candidate's freed registry port is still probed, and if a
// FOREIGN local service rebound that port the router would accept + CACHE that
// foreign tool catalog — a correctness + trust-boundary defect.
//
// These complement the existing operator-stop tool-call falsification
// (serena_idle_sweeper_test.go TestRouterWake_OperatorStopRefused_*) and the
// multi-candidate wake-continuation tests; here the surface under test is the
// tools/list fetch-loop EXCLUSION, not the wake refusal itself.
package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestRouterToolsList_OperatorStopRefused_ForeignPortNotProbedOrCached is the
// falsifying regression for r37-2 P2.
//
// Setup: one serena workspace whose WakeIdleFn refuses with
// ErrWakeRefusedOperatorStop. Its UpstreamURLFn resolves to a FOREIGN responder
// (modelling a different local service that rebound the freed registry port)
// that would happily answer tools/list with a foreign catalog. The operator
// stopped the real serena daemon, so this port must NEVER be probed by the
// router's tools/list fetch loop.
//
// Pre-fix (wakeOneSerenaCandidateForToolsList only logged + continued, and
// handleToolsList passed the ORIGINAL entries to fetchToolsListFromAnyDaemon):
// the operator-stopped candidate is probed, the foreign daemon answers, and the
// foreign tool catalog is returned to the client AND cached — this test FAILS
// (the foreign tool appears + the foreign daemon is hit).
//
// Post-fix: the candidate is excluded from the fetch loop. With no eligible
// daemon left, tools/list fails loud + retryable (all-operator-stopped error),
// the foreign daemon is never hit, and nothing is cached.
func TestRouterToolsList_OperatorStopRefused_ForeignPortNotProbedOrCached(t *testing.T) {
	const wsPath = "/proj/operator-stopped"
	ws := serenaWS("operator-stopped", wsPath, 9320)

	// The FOREIGN responder: a process that rebound the operator-stopped
	// daemon's freed registry port. It completes a serena-shaped handshake and
	// answers tools/list with a catalog the router must never surface.
	foreign := newFakeSerenaDaemon("foreign")
	var foreignToolHits int
	foreign.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		foreignToolHits++
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("foreign upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"forbidden_foreign_tool"}]}}`))
	}
	foreignTS := httptest.NewServer(foreign.handler())
	t.Cleanup(foreignTS.Close)

	// Track every port the wake-loop liveness probe is asked about. The
	// operator-stopped candidate must never reach the fetch loop; this also
	// guards against any future regression that would probe the freed port for
	// liveness during the wake.
	var probedPorts []int
	origPortLive := serenaToolsListPortLiveFn
	serenaToolsListPortLiveFn = func(_ context.Context, port int) bool {
		probedPorts = append(probedPorts, port)
		// Model the foreign rebind: the freed port now accepts TCP.
		return true
	}
	t.Cleanup(func() { serenaToolsListPortLiveFn = origPortLive })

	var wakeCalls int
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return foreignTS.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			wakeCalls++
			return api.ErrWakeRefusedOperatorStop
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}
	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	rr := postSerena(t, s, body, hdr)
	// JSON-RPC errors are in-band at HTTP 200.
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200 (JSON-RPC error in-band); body=%s", rr.Code, rr.Body.String())
	}
	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn calls = %d, want 1 (the single operator-stopped candidate)", wakeCalls)
	}

	// CORE assertion: the foreign daemon's tools/list must NEVER be probed.
	if foreignToolHits != 0 {
		t.Fatalf("operator-stopped candidate's foreign port was probed for tools/list %d time(s); want 0 (the candidate must be excluded from the fetch loop)", foreignToolHits)
	}

	// The router must NOT surface the foreign catalog; it must fail loud +
	// retryable because every candidate is operator-stopped.
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list body: %v; body=%s", err, rr.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("tools/list returned a result with no error; foreign catalog leaked? body=%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "forbidden_foreign_tool") {
		t.Fatalf("tools/list surfaced the FOREIGN tool catalog from an operator-stopped port; body=%s", rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(resp.Error.Message), "operator stop") {
		t.Fatalf("error message = %q, want it to name the operator stop", resp.Error.Message)
	}

	// Cache must NOT have been poisoned: a second tools/list must again fail
	// (no cached foreign catalog served), and the foreign daemon still untouched.
	rr2 := postSerena(t, s, body, hdr)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second tools/list status = %d, want 200", rr2.Code)
	}
	if strings.Contains(rr2.Body.String(), "forbidden_foreign_tool") {
		t.Fatalf("second tools/list served a CACHED foreign catalog; body=%s", rr2.Body.String())
	}
	if foreignToolHits != 0 {
		t.Fatalf("foreign port probed after second call = %d; want still 0", foreignToolHits)
	}
}

// TestRouterToolsList_TransientWakeError_CandidateStaysEligible is the negative
// control: a NON-operator-stop wake error (respawn not ready in time) is
// TRANSIENT, not terminal. The candidate must stay eligible in the fetch loop
// so the existing wake-as-an-optimization posture is preserved — a daemon that
// was not ready when the wake ran may answer by the time the fetch loop reaches
// it. The fix must distinguish ErrWakeRefusedOperatorStop (exclude) from every
// other wake error (keep eligible) via errors.Is.
func TestRouterToolsList_TransientWakeError_CandidateStaysEligible(t *testing.T) {
	const wsPath = "/proj/transient"
	ws := serenaWS("transient", wsPath, 9321)

	daemon := newFakeSerenaDaemon("transient")
	var toolHits int
	var mu sync.Mutex
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		mu.Lock()
		toolHits++
		mu.Unlock()
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			// A transient not-ready error — NOT an operator-stop refusal. The
			// candidate must remain eligible for the fetch loop.
			return context.DeadlineExceeded
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("transient-wake-error tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// The candidate stayed eligible and answered the real catalog.
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	mu.Lock()
	got := toolHits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("daemon tool hits = %d, want 1 (transient wake error must keep the candidate eligible for the fetch)", got)
	}
}
