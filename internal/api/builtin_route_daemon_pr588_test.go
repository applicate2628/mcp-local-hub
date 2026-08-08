// internal/api/builtin_route_daemon_pr588_test.go
//
// Regression coverage for the two codex bot PR #588 P2 findings that live in
// package api:
//
//   - the reserved `route` server name was enforced only against SHIPPED
//     manifests, so an operator/dev manifest could still claim it;
//   - the built-in route front daemon was fed to the GENERIC /mcp health
//     probe, which it structurally cannot answer.
//
// Both tests are hermetic: no scheduler, no state dir, no live fleet.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestCheckManifestName_RejectsReservedRouteServerName is the other half of
// the reservation TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest
// covers.
//
// buildMergedSupervisorIntent keys per-server ownership by
// SupervisorDaemon.Server, so a manifest named "route" would make its own
// install/uninstall claim the built-in front-daemon row and drop it until the
// next cold supervisor restart. Worse, a `route`/`front` daemon inside such a
// manifest carries the SAME (Server, Daemon) identity pair, so
// EnsureBuiltinRouteDaemon's collision guard — which fires on identity
// MISMATCH — would stay silent while the foreign command replaced the built-in
// one.
func TestCheckManifestName_RejectsReservedRouteServerName(t *testing.T) {
	err := checkManifestName(BuiltinRouteServer)
	if err == nil {
		t.Fatalf("checkManifestName(%q) must reject the reserved built-in route server name; it returned nil, so an operator manifest could claim the front daemon's supervisor-intent identity", BuiltinRouteServer)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("the refusal must tell the operator the name is reserved; got %v", err)
	}

	// Control: the reservation is exact, not a prefix/substring sweep. A
	// neighbouring name must still be accepted, or the gate would quietly
	// shrink the legal manifest namespace.
	for _, ok := range []string{"router", "route-extras", "myroute"} {
		if cerr := checkManifestName(ok); cerr != nil {
			t.Fatalf("checkManifestName(%q) must still be accepted (the reservation is the exact name %q only); got %v", ok, BuiltinRouteServer, cerr)
		}
	}
}

// TestProbeDaemonHealth_RouteFrontRowUsesRouteSpecificProbe pins the probe
// dispatch.
//
// The route daemon's descriptor is an ordinary global row (nonzero Port, not
// maintenance-classified), so mergeSupervisorOnlyDaemonRows feeds it into
// StatusWithOpts(ProbeHealth=true). The generic probe POSTs initialize +
// tools/list to /mcp — an endpoint RouteHandler never mounts — so a healthy
// front daemon rendered as a FAILED probe.
func TestProbeDaemonHealth_RouteFrontRowUsesRouteSpecificProbe(t *testing.T) {
	origSingle := singleHealthProbeFn
	origRoute := routeFrontHealthProbeFn
	t.Cleanup(func() {
		singleHealthProbeFn = origSingle
		routeFrontHealthProbeFn = origRoute
	})

	var mu sync.Mutex
	var singlePorts, routePorts []int
	singleHealthProbeFn = func(port int) *HealthProbe {
		mu.Lock()
		defer mu.Unlock()
		singlePorts = append(singlePorts, port)
		return &HealthProbe{OK: true, ToolCount: 7}
	}
	routeFrontHealthProbeFn = func(port int) *HealthProbe {
		mu.Lock()
		defer mu.Unlock()
		routePorts = append(routePorts, port)
		return &HealthProbe{OK: true, Source: RouteFrontHealthSource}
	}

	rows := []DaemonStatus{
		{TaskName: BuiltinRouteTaskName, Server: BuiltinRouteServer, Daemon: BuiltinRouteDaemonName, Port: 9137, State: "Running"},
		// Same row without the canonical leading backslash — the dispatch must
		// canonicalize, not string-compare raw.
		{TaskName: strings.TrimPrefix(BuiltinRouteTaskName, `\`), Server: BuiltinRouteServer, Daemon: BuiltinRouteDaemonName, Port: 9138, State: "Running"},
		{TaskName: `\mcp-local-hub-fetch-default`, Server: "fetch", Daemon: "default", Port: 9130, State: "Running"},
	}
	probeDaemonHealth(rows)

	mu.Lock()
	defer mu.Unlock()
	sortedInts := func(in []int) []int {
		out := append([]int(nil), in...)
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if out[j] < out[i] {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}
	gotRoute := sortedInts(routePorts)
	if len(gotRoute) != 2 || gotRoute[0] != 9137 || gotRoute[1] != 9138 {
		t.Fatalf("both built-in route rows (canonical and bare task name) must be probed through the route-specific probe; routeFrontHealthProbeFn saw ports %v", routePorts)
	}
	if len(singlePorts) != 1 || singlePorts[0] != 9130 {
		t.Fatalf("the generic /mcp probe must still serve every NON-route row, and only those; singleHealthProbeFn saw ports %v (a route port here means a healthy front daemon would be reported as a failed probe)", singlePorts)
	}
	if rows[0].Health == nil || rows[0].Health.Source != RouteFrontHealthSource {
		t.Fatalf("the route row's probe result must be tagged %q so no renderer prints a phantom tool count; got %+v", RouteFrontHealthSource, rows[0].Health)
	}
	if rows[2].Health == nil || rows[2].Health.ToolCount != 7 {
		t.Fatalf("the non-route row must keep the generic probe's result; got %+v", rows[2].Health)
	}
}

// TestRouteFrontHealthProbe_RejectsBrokenLaterLSPRoute proves health uses the
// same complete manifest-owned predicate as client cutover. A healthy first
// LSP route cannot mask a broken later sibling.
func TestRouteFrontHealthProbe_RejectsBrokenLaterLSPRoute(t *testing.T) {
	specs, err := loadLSPRouterLanguageSpecs(nil)
	if err != nil || len(specs) < 2 {
		t.Fatalf("load at least two canonical LSP route specs: len=%d err=%v", len(specs), err)
	}
	firstPath := fmt.Sprintf(lspRouterURLPathTemplate, specs[0].Name)
	brokenPath := fmt.Sprintf(lspRouterURLPathTemplate, specs[1].Name)
	mux := http.NewServeMux()
	mux.HandleFunc(SerenaRouterURLPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			// The router's non-POST signature: 405 + Allow: POST, DELETE.
			w.Header().Set("Allow", "POST, DELETE")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Mcp-Session-Id", "route-test-session")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}},
		})
	})
	mux.HandleFunc(firstPath, serveMCPFrontRoutesReadiness)
	// Everything else — including /mcp — 404s, exactly as RouteHandler does.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	if generic := singleHealthProbe(port); generic.OK {
		t.Fatalf("precondition broken: the generic /mcp probe should NOT succeed against a route-shaped listener; got %+v", generic)
	}
	routeProbe := routeFrontHealthProbe(port)
	if routeProbe.OK {
		t.Fatalf("a front with healthy Serena and first LSP routes must remain unhealthy when later route %s is broken; got %+v", brokenPath, routeProbe)
	}
	if routeProbe.Source != RouteFrontHealthSource {
		t.Fatalf("route probe result must carry Source=%q; got %q", RouteFrontHealthSource, routeProbe.Source)
	}
	if !strings.Contains(strings.ToLower(routeProbe.Err), "lsp") {
		t.Fatalf("the failed health probe must name the LSP stage so operators can diagnose the route gap; got %q", routeProbe.Err)
	}
	if !strings.Contains(routeProbe.Err, specs[1].Name) || !strings.Contains(routeProbe.Err, string(MCPFrontProbeStageShapeResponse)) {
		t.Fatalf("health detail must identify the broken later language and exact probe stage; got %q", routeProbe.Err)
	}
}
