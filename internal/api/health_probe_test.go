package api

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type healthProbeRoundTripper func(*http.Request) (*http.Response, error)

func (f healthProbeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type failingHealthProbeBody struct{ err error }

func (b failingHealthProbeBody) Read([]byte) (int, error) { return 0, b.err }
func (failingHealthProbeBody) Close() error               { return nil }

func healthProbeResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}
}

// TestSingleHealthProbe_OK verifies the probe reports OK + correct
// tool count when the MCP server responds to initialize + tools/list.
func TestSingleHealthProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if got := r.Header.Get("Mcp-Session-Id"); got != "test-session" {
				t.Errorf("cleanup session header = %q, want test-session", got)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body := ""
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		switch {
		case strings.Contains(body, `"initialize"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case strings.Contains(body, `"tools/list"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a"},{"name":"b"},{"name":"c"}]}}`))
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("singleHealthProbe returned nil")
	}
	if !h.OK {
		t.Errorf("expected OK=true, got %+v", h)
	}
	if h.ToolCount != 3 {
		t.Errorf("ToolCount = %d, want 3", h.ToolCount)
	}
}

// TestSingleHealthProbe_ErrorFromServer verifies the probe reports
// the MCP server's error verbatim when tools/list returns a JSON-RPC
// error. This is the scenario the audit flagged: daemon alive, MCP
// server up, but backend (e.g. gdb binary) missing — the server
// responds to tools/list with an error and we want that visible.
func TestSingleHealthProbe_ErrorFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		if strings.Contains(body, `"initialize"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		// tools/list returns an error.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"backend unavailable: gdb not on PATH"}}`))
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Errorf("expected OK=false, got %+v", h)
	}
	if !strings.Contains(h.Err, "gdb not on PATH") {
		t.Errorf("expected err to include upstream message: %q", h.Err)
	}
}

// TestSingleHealthProbe_Unreachable verifies the probe reports an
// error (not nil, not OK) for a port nothing is listening on.
func TestSingleHealthProbe_Unreachable(t *testing.T) {
	// A port that almost certainly isn't bound.
	h := singleHealthProbe(65535)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Errorf("expected OK=false for unreachable port: %+v", h)
	}
	if h.Err == "" {
		t.Error("expected non-empty Err")
	}
}

// TestSingleHealthProbe_OversizedResponse verifies tools/list responses
// larger than the safety cap are rejected without full buffering.
func TestSingleHealthProbe_OversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		if strings.Contains(body, `"initialize"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		tooLarge := strings.Repeat("x", maxHealthProbeResponseBytes+1)
		_, _ = w.Write([]byte(tooLarge))
	}))
	defer srv.Close()
	port := parsePort(t, srv.URL)

	h := singleHealthProbe(port)
	if h == nil {
		t.Fatal("nil probe")
	}
	if h.OK {
		t.Fatalf("expected OK=false for oversized response, got %+v", h)
	}
	if !strings.Contains(h.Err, "response too large") {
		t.Fatalf("expected oversized-response error, got %q", h.Err)
	}
}

func TestSingleHealthProbe_SessionCleanup_AllPostInitializeReturns(t *testing.T) {
	cases := []struct {
		name    string
		list    func() (*http.Response, error)
		wantErr string
	}{
		{
			name: "tools list transport failure",
			list: func() (*http.Response, error) {
				return nil, errors.New("synthetic tools transport failure")
			},
			wantErr: "synthetic tools transport failure",
		},
		{
			name: "tools list body read failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, failingHealthProbeBody{err: errors.New("synthetic body read failure")}), nil
			},
			wantErr: "tools/list: read: synthetic body read failure",
		},
		{
			name: "tools list oversized response",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(strings.Repeat("x", maxHealthProbeResponseBytes+1)))), nil
			},
			wantErr: "response too large",
		},
		{
			name: "tools list parse failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader("not-json"))), nil
			},
			wantErr: "tools/list: parse:",
		},
		{
			name: "tools list JSON RPC failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"synthetic rpc failure"}}`))), nil
			},
			wantErr: "synthetic rpc failure",
		},
		{
			name: "successful tools list",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[{"name":"one"}]}}`))), nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := healthProbeHTTPClient
			defer func() { healthProbeHTTPClient = previous }()
			var deleteSessions []string
			healthProbeHTTPClient = func() *http.Client {
				return &http.Client{Transport: healthProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
					switch req.Method {
					case http.MethodPost:
						body, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatalf("read request body: %v", err)
						}
						if strings.Contains(string(body), `"initialize"`) {
							resp := healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{}}`)))
							resp.Header.Set("Mcp-Session-Id", "issued-session")
							return resp, nil
						}
						return tc.list()
					case http.MethodDelete:
						deleteSessions = append(deleteSessions, req.Header.Get("Mcp-Session-Id"))
						return healthProbeResponse(http.StatusNoContent, io.NopCloser(strings.NewReader(""))), nil
					default:
						t.Fatalf("unexpected method %s", req.Method)
						return nil, nil
					}
				})}
			}

			h := singleHealthProbe(1)
			if h == nil {
				t.Fatal("nil health probe")
			}
			if len(deleteSessions) != 1 || deleteSessions[0] != "issued-session" {
				t.Fatalf("DELETE sessions = %q, want exactly issued-session", deleteSessions)
			}
			if tc.wantErr == "" {
				if !h.OK || h.ToolCount != 1 {
					t.Fatalf("successful probe = %+v", h)
				}
			} else if h.OK || !strings.Contains(h.Err, tc.wantErr) {
				t.Fatalf("failed probe = %+v, want error containing %q", h, tc.wantErr)
			}
		})
	}
}

func TestSingleHealthProbe_SessionCleanupFailureIsVisibleAndPreservesPrimary(t *testing.T) {
	drainFailure := errors.New("distinct cleanup drain failure")
	closeFailure := errors.New("distinct cleanup close failure")
	for _, tc := range []struct {
		name            string
		listBody        string
		cleanupCode     int
		cleanupDrainErr error
		cleanupCloseErr error
		wantPrimary     string
		wantCleanup     string
	}{
		{name: "cleanup failure after successful probe", listBody: `{"jsonrpc":"2.0","result":{"tools":[]}}`, cleanupCode: http.StatusBadGateway},
		{name: "cleanup failure retains tools list failure", listBody: `{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary tools failure"}}`, cleanupCode: http.StatusBadGateway, wantPrimary: "primary tools failure"},
		{name: "drain failure after successful probe", listBody: `{"jsonrpc":"2.0","result":{"tools":[]}}`, cleanupCode: http.StatusNoContent, cleanupDrainErr: drainFailure, wantCleanup: drainFailure.Error()},
		{name: "close failure retains tools list failure first", listBody: `{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary tools failure"}}`, cleanupCode: http.StatusNoContent, cleanupCloseErr: closeFailure, wantPrimary: "primary tools failure", wantCleanup: closeFailure.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := healthProbeHTTPClient
			defer func() { healthProbeHTTPClient = previous }()
			healthProbeHTTPClient = func() *http.Client {
				return &http.Client{Transport: healthProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
					switch req.Method {
					case http.MethodPost:
						body, _ := io.ReadAll(req.Body)
						if strings.Contains(string(body), `"initialize"`) {
							resp := healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{}}`)))
							resp.Header.Set("Mcp-Session-Id", "issued-session")
							return resp, nil
						}
						return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(tc.listBody))), nil
					case http.MethodDelete:
						var body io.ReadCloser = io.NopCloser(strings.NewReader("cleanup rejected"))
						if tc.cleanupDrainErr != nil || tc.cleanupCloseErr != nil {
							body = &failingCleanupBody{drainErr: tc.cleanupDrainErr, closeErr: tc.cleanupCloseErr}
						}
						return healthProbeResponse(tc.cleanupCode, body), nil
					default:
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
				})}
			}
			h := singleHealthProbe(1)
			if h == nil || h.OK || !strings.Contains(h.Err, "MCP_HEALTH_SESSION_CLEANUP_FAILED") {
				t.Fatalf("cleanup failure probe = %+v", h)
			}
			if tc.wantCleanup != "" && !strings.Contains(h.Err, tc.wantCleanup) {
				t.Fatalf("cleanup cause %q is not visible: %q", tc.wantCleanup, h.Err)
			}
			if tc.wantPrimary != "" {
				if !strings.Contains(h.Err, tc.wantPrimary) || strings.Index(h.Err, tc.wantPrimary) > strings.Index(h.Err, "MCP_HEALTH_SESSION_CLEANUP_FAILED") {
					t.Fatalf("primary failure was not retained first: %q", h.Err)
				}
			}
		})
	}
}

func TestSingleHealthProbe_SessionCleanup404And405PreserveHealthyResult(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			previous := healthProbeHTTPClient
			defer func() { healthProbeHTTPClient = previous }()
			healthProbeHTTPClient = func() *http.Client {
				return &http.Client{Transport: healthProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
					switch req.Method {
					case http.MethodPost:
						body, _ := io.ReadAll(req.Body)
						if strings.Contains(string(body), `"initialize"`) {
							resp := healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{}}`)))
							resp.Header.Set("Mcp-Session-Id", "issued-session")
							return resp, nil
						}
						return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[{}]}}`))), nil
					case http.MethodDelete:
						return healthProbeResponse(status, io.NopCloser(strings.NewReader("already gone or unsupported"))), nil
					default:
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
				})}
			}
			if got := singleHealthProbe(1); got == nil || !got.OK || got.ToolCount != 1 {
				t.Fatalf("cleanup HTTP %d changed primary health result: %+v", status, got)
			}
		})
	}
}

func TestSingleHealthProbe_DoesNotCleanupWithoutAdmittedSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initStatus  int
		withSession bool
	}{
		{name: "initialize HTTP failure", initStatus: http.StatusBadGateway, withSession: true},
		{name: "successful initialize without session ID", initStatus: http.StatusOK, withSession: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := healthProbeHTTPClient
			defer func() { healthProbeHTTPClient = previous }()
			deleteCalls := 0
			healthProbeHTTPClient = func() *http.Client {
				return &http.Client{Transport: healthProbeRoundTripper(func(req *http.Request) (*http.Response, error) {
					switch req.Method {
					case http.MethodPost:
						body, _ := io.ReadAll(req.Body)
						if strings.Contains(string(body), `"initialize"`) {
							resp := healthProbeResponse(tc.initStatus, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{}}`)))
							if tc.withSession {
								resp.Header.Set("Mcp-Session-Id", "not-admitted")
							}
							return resp, nil
						}
						return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[]}}`))), nil
					case http.MethodDelete:
						deleteCalls++
						return healthProbeResponse(http.StatusNoContent, io.NopCloser(strings.NewReader(""))), nil
					default:
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
				})}
			}
			h := singleHealthProbe(1)
			if h == nil || (tc.initStatus >= http.StatusBadRequest && h.Err == "") {
				t.Fatalf("probe = %+v", h)
			}
			if deleteCalls != 0 {
				t.Fatalf("DELETE calls = %d, want 0 without an admitted session", deleteCalls)
			}
		})
	}
}

// parsePort is a small helper: extract the port part from an
// httptest.Server URL ("http://127.0.0.1:PORT").
func parsePort(t *testing.T, url string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("split host:port from %q: %v", url, err)
	}
	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return p
}
