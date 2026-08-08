package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

func TestAssertMCPFrontRoutesLive_ReleasesEveryIssuedProbeSession(t *testing.T) {
	var mu sync.Mutex
	active := map[string]struct{}{}
	nextID := 0
	specs, err := loadLSPRouterLanguageSpecs(nil)
	if err != nil || len(specs) == 0 {
		t.Fatal("canonical manifest has no LSP routes")
	}
	broken := fmt.Sprintf(lspRouterURLPathTemplate, specs[0].Name)

	for _, tc := range []struct {
		name    string
		broken  bool
		wantErr bool
	}{
		{name: "all routes live"},
		{name: "later route failure", broken: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			clear(active)
			nextID = 0
			mu.Unlock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.broken && r.URL.Path == broken && r.Method == http.MethodHead {
					http.NotFound(w, r)
					return
				}
				if r.Method == http.MethodDelete {
					id := r.Header.Get("Mcp-Session-Id")
					mu.Lock()
					_, known := active[id]
					delete(active, id)
					mu.Unlock()
					if !known {
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", "POST, DELETE")
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				mu.Lock()
				nextID++
				id := fmt.Sprintf("probe-%d", nextID)
				active[id] = struct{}{}
				mu.Unlock()
				w.Header().Set("Mcp-Session-Id", id)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			defer server.Close()
			parsed, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(parsed.Port())
			if err != nil {
				t.Fatal(err)
			}
			err = AssertMCPFrontRoutesLive(context.Background(), port)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(active) != 0 {
				t.Fatalf("probe-created sessions leaked: %v", active)
			}
		})
	}
}

func TestRouterProbeErrorStages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shape-response", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/shape-post-only", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/initialize-http", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	mux.HandleFunc("/initialize-json", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "not-json") })
	mux.HandleFunc("/initialize-rpc", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"down"}}`)
	})
	mux.HandleFunc("/initialize-missing", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"jsonrpc":"2.0","id":1}`) })
	mux.HandleFunc("/initialize-missing-session", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`) })
	mux.HandleFunc("/initialize-delete-405", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Mcp-Session-Id", "synthetic-session")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
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
		{"shape_post_only", MCPFrontProbeStageShapeResponse, func() error { return routerRouteShapeProbe(context.Background(), port, "/shape-post-only") }},
		{"initialize_http", MCPFrontProbeStageInitializeHTTPStatus, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-http") }},
		{"initialize_json", MCPFrontProbeStageInitializeJSONDecode, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-json") }},
		{"initialize_jsonrpc", MCPFrontProbeStageInitializeJSONRPCError, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-rpc") }},
		{"initialize_missing_result", MCPFrontProbeStageInitializeResultMissing, func() error { return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-missing") }},
		{"initialize_missing_session", MCPFrontProbeStageInitializeSessionIDMissing, func() error {
			return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-missing-session")
		}},
		{"initialize_delete_405", MCPFrontProbeStageSessionCleanupResponse, func() error {
			return routerInitializeLifecycleProbe(context.Background(), port, "/initialize-delete-405")
		}},
		{"initialize_delete_body_settlement", MCPFrontProbeStageSessionCleanupTransport, func() error {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.Method {
				case http.MethodPost:
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Mcp-Session-Id": []string{"synthetic-session"}},
						Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{}}`)),
					}, nil
				case http.MethodDelete:
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Header:     make(http.Header),
						Body: &failingCleanupBody{
							drainErr: errors.New("route cleanup drain failure"),
							closeErr: errors.New("route cleanup close failure"),
						},
					}, nil
				default:
					return nil, fmt.Errorf("unexpected method %s", req.Method)
				}
			})}
			return routerInitializeLifecycleProbeWithClient(context.Background(), port, "/initialize-delete-body-settlement", client)
		}},
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

func TestRouterInitializeLifecycleProbe_ClosesInitializeBodyBeforeSessionDelete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		body       string
		wantErr    bool
	}{
		{name: "valid result", statusCode: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{name: "primary response error", statusCode: http.StatusBadGateway, body: `down`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initializeClosed := false
			deleteCalls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.Method {
				case http.MethodPost:
					return &http.Response{
						StatusCode: tc.statusCode,
						Header:     http.Header{"Mcp-Session-Id": []string{"synthetic-session"}},
						Body:       closeTracker{ReadCloser: io.NopCloser(strings.NewReader(tc.body)), closed: &initializeClosed},
					}, nil
				case http.MethodDelete:
					deleteCalls++
					if !initializeClosed {
						return nil, fmt.Errorf("session DELETE ran before initialize body Close")
					}
					return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
				default:
					return nil, fmt.Errorf("unexpected method %s", r.Method)
				}
			})}

			err := routerInitializeLifecycleProbeWithClient(context.Background(), 9137, "/mcp", client)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if !initializeClosed || deleteCalls != 1 {
				t.Fatalf("initializeClosed=%v deleteCalls=%d, want true/1; err=%v", initializeClosed, deleteCalls, err)
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
	if r.Method == http.MethodDelete {
		if r.Header.Get("Mcp-Session-Id") == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Mcp-Session-Id", "readiness-probe-session")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mcp-front-readiness","version":"test"}}}`)
}
