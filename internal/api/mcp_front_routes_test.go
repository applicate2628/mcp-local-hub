package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAssertMCPFrontRoutesLive(t *testing.T) {
	t.Run("both_routes_live", func(t *testing.T) {
		port, cleanup := startMCPFrontRoutesReadinessServer(t, nil)
		defer cleanup()

		if err := AssertMCPFrontRoutesLive(context.Background(), port); err != nil {
			t.Fatalf("combined front readiness rejected two live routes: %v", err)
		}
	})

	t.Run("later_lsp_route_unavailable_reports_exact_target_and_stage", func(t *testing.T) {
		specs, err := loadLSPRouterLanguageSpecs(nil)
		if err != nil || len(specs) < 2 {
			t.Fatalf("load at least two canonical LSP route specs: len=%d err=%v", len(specs), err)
		}
		broken := specs[1]
		brokenPath := fmt.Sprintf(lspRouterURLPathTemplate, broken.Name)
		port, cleanup := startMCPFrontRoutesReadinessServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == brokenPath {
				http.NotFound(w, r)
				return true
			}
			return false
		})
		defer cleanup()

		err = AssertMCPFrontRoutesLive(context.Background(), port)
		if err == nil {
			t.Fatal("combined front readiness accepted a broken later LSP route after a healthy first route")
		}
		assertMCPFrontReadinessError(t, err, MCPFrontRouteStageLSP, MCPFrontProbeStageShapeResponse, broken.Name, broken.Backend)
	})

	t.Run("gopls_backend_jsonrpc_rejection_reports_exact_substage", func(t *testing.T) {
		specs, err := loadLSPRouterLanguageSpecs(nil)
		if err != nil {
			t.Fatalf("load canonical LSP route specs: %v", err)
		}
		var goplsLanguage, goplsBackend string
		for _, spec := range specs {
			if spec.Backend == "gopls-mcp" {
				goplsLanguage, goplsBackend = spec.Name, spec.Backend
				break
			}
		}
		if goplsLanguage == "" {
			t.Fatal("canonical manifest must retain a gopls-mcp route for this backend-axis regression guard")
		}
		brokenPath := fmt.Sprintf(lspRouterURLPathTemplate, goplsLanguage)
		port, cleanup := startMCPFrontRoutesReadinessServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == brokenPath && r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"gopls route unavailable"}}`)
				return true
			}
			return false
		})
		defer cleanup()

		err = AssertMCPFrontRoutesLive(context.Background(), port)
		if err == nil {
			t.Fatal("combined front readiness accepted a JSON-RPC failure from the gopls backend route")
		}
		assertMCPFrontReadinessError(t, err, MCPFrontRouteStageLSP, MCPFrontProbeStageInitializeJSONRPCError, goplsLanguage, goplsBackend)
	})

	t.Run("non_positive_port_reports_front_input_stage", func(t *testing.T) {
		err := AssertMCPFrontRoutesLive(context.Background(), 0)
		if err == nil {
			t.Fatal("combined front readiness accepted a non-positive port")
		}
		assertMCPFrontReadinessError(t, err, MCPFrontRouteStageFront, MCPFrontProbeStageInput, "", "")
	})
}

func TestRouterProbeErrorStages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shape-response", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/initialize-http", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	mux.HandleFunc("/initialize-json", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "not-json") })
	mux.HandleFunc("/initialize-rpc", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"down"}}`)
	})
	mux.HandleFunc("/initialize-missing", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"jsonrpc":"2.0","id":1}`) })
	srv := httptest.NewServer(mux)
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("parse probe-stage server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		srv.Close()
		t.Fatalf("parse probe-stage server port: %v", err)
	}
	defer srv.Close()

	tests := []struct {
		name  string
		stage MCPFrontProbeStage
		probe func() error
	}{
		{"shape_response", MCPFrontProbeStageShapeResponse, func() error { return routerRouteShapeProbe(context.Background(), port, "/shape-response") }},
		{"initialize_http", MCPFrontProbeStageInitializeHTTPStatus, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-http") }},
		{"initialize_json", MCPFrontProbeStageInitializeJSONDecode, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-json") }},
		{"initialize_jsonrpc", MCPFrontProbeStageInitializeJSONRPCError, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-rpc") }},
		{"initialize_missing_result", MCPFrontProbeStageInitializeResultMissing, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-missing") }},
		{"parent_canceled", MCPFrontProbeStageParentCanceled, func() error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return routerRouteShapeProbe(ctx, port, "/shape-response")
		}},
		{"parent_deadline", MCPFrontProbeStageParentDeadline, func() error {
			ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
			defer cancel()
			return routerRouteShapeProbe(ctx, port, "/shape-response")
		}},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve closed transport port: %v", err)
	}
	closedPort := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved transport port: %v", err)
	}
	tests = append(tests, struct {
		name  string
		stage MCPFrontProbeStage
		probe func() error
	}{"shape_transport", MCPFrontProbeStageShapeTransport, func() error {
		return routerRouteShapeProbe(context.Background(), closedPort, "/closed")
	}})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.probe()
			var probeErr *routerProbeError
			if !errors.As(err, &probeErr) || probeErr.Stage != test.stage {
				t.Fatalf("probe error=%T %v stage=%q, want typed stage %q", err, err, probeStageFromError(err), test.stage)
			}
		})
	}
}

func assertMCPFrontReadinessError(t *testing.T, err error, route MCPFrontRouteStage, probe MCPFrontProbeStage, language, backend string) {
	t.Helper()
	var readinessErr *MCPFrontRoutesLiveError
	if !errors.As(err, &readinessErr) {
		t.Fatalf("combined readiness error must preserve its typed detail: %T %v", err, err)
	}
	if readinessErr.Code != MCPFrontRouteNotReadyCode || readinessErr.Stage != route || readinessErr.ProbeStage != probe || readinessErr.Language != language || readinessErr.Backend != backend {
		t.Fatalf("readiness detail=(code=%q route=%q probe=%q language=%q backend=%q), want (%q %q %q %q %q); err=%v",
			readinessErr.Code, readinessErr.Stage, readinessErr.ProbeStage, readinessErr.Language, readinessErr.Backend,
			MCPFrontRouteNotReadyCode, route, probe, language, backend, err)
	}
	if !strings.Contains(err.Error(), MCPFrontRouteNotReadyCode) {
		t.Fatalf("human-readable error must retain stable discriminator %q: %v", MCPFrontRouteNotReadyCode, err)
	}
	if route == MCPFrontRouteStageLSP && !errors.Is(err, ErrLSPRouterRouteNotLive) {
		t.Fatalf("combined readiness must preserve the LSP route sentinel: %v", err)
	}
}

func startMCPFrontRoutesReadinessServer(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) (port int, cleanup func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(SerenaRouterURLPath, serveMCPFrontRoutesReadiness)
	mux.HandleFunc("/lsp/", func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r) {
			return
		}
		serveMCPFrontRoutesReadiness(w, r)
	})

	srv := httptest.NewServer(mux)
	u, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("parse readiness server URL: %v", err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		srv.Close()
		t.Fatalf("parse readiness server port: %v", err)
	}
	return port, srv.Close
}

func serveMCPFrontRoutesReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mcp-front-readiness","version":"test"}}}`)
}
