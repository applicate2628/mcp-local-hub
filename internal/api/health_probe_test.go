package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/mcpcompat/readinesswire"
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

// TestSingleHealthProbe_ErrorFromServer verifies the probe projects a closed
// failure id and does not expose an arbitrary child error body.
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
	if h.Readiness.FailureID != "MCP_COMPAT_CHILD_RESPONSE_INVALID" || strings.Contains(h.Err, "gdb not on PATH") {
		t.Errorf("expected closed redacted failure: %+v", h)
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
	if h.Readiness.FailureID != "MCP_COMPAT_CHILD_RESPONSE_INVALID" || strings.Contains(h.Err, strings.Repeat("x", 16)) {
		t.Fatalf("expected closed oversized-response failure, got %+v", h)
	}
}

func TestSingleHealthProbe_SessionCleanup_AllPostInitializeReturns(t *testing.T) {
	cases := []struct {
		name          string
		list          func() (*http.Response, error)
		wantFailureID string
	}{
		{
			name: "tools list transport failure",
			list: func() (*http.Response, error) {
				return nil, errors.New("synthetic tools transport failure")
			},
			wantFailureID: "MCP_READINESS_HOST_UNAVAILABLE",
		},
		{
			name: "tools list body read failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, failingHealthProbeBody{err: errors.New("synthetic body read failure")}), nil
			},
			wantFailureID: "MCP_COMPAT_CHILD_RESPONSE_INVALID",
		},
		{
			name: "tools list oversized response",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(strings.Repeat("x", maxHealthProbeResponseBytes+1)))), nil
			},
			wantFailureID: "MCP_COMPAT_CHILD_RESPONSE_INVALID",
		},
		{
			name: "tools list parse failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader("not-json"))), nil
			},
			wantFailureID: "MCP_COMPAT_CHILD_RESPONSE_INVALID",
		},
		{
			name: "tools list JSON RPC failure",
			list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"synthetic rpc failure"}}`))), nil
			},
			wantFailureID: "MCP_COMPAT_CHILD_RESPONSE_INVALID",
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
			if tc.wantFailureID == "" {
				if !h.OK || h.ToolCount != 1 {
					t.Fatalf("successful probe = %+v", h)
				}
			} else if h.OK || h.Readiness.FailureID != tc.wantFailureID {
				t.Fatalf("failed probe = %+v, want failure id %q", h, tc.wantFailureID)
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
		wantFailureID   string
		wantStage       string
		wantSecondary   bool
		forbiddenRaw    []string
	}{
		{name: "cleanup failure after successful probe", listBody: `{"jsonrpc":"2.0","result":{"tools":[]}}`, cleanupCode: http.StatusBadGateway, wantFailureID: "MCP_HEALTH_SESSION_CLEANUP_FAILED", wantStage: MCPReadinessStageSessionCleanup},
		{name: "cleanup failure preserves tools list primary", listBody: `{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary tools failure"}}`, cleanupCode: http.StatusBadGateway, wantFailureID: readinesswire.FailureChildResponseInvalid, wantStage: MCPReadinessStageToolsList, wantSecondary: true, forbiddenRaw: []string{"primary tools failure"}},
		{name: "drain failure after successful probe", listBody: `{"jsonrpc":"2.0","result":{"tools":[]}}`, cleanupCode: http.StatusNoContent, cleanupDrainErr: drainFailure, wantFailureID: "MCP_HEALTH_SESSION_CLEANUP_FAILED", wantStage: MCPReadinessStageSessionCleanup, forbiddenRaw: []string{drainFailure.Error()}},
		{name: "close failure preserves tools list primary", listBody: `{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary tools failure"}}`, cleanupCode: http.StatusNoContent, cleanupCloseErr: closeFailure, wantFailureID: readinesswire.FailureChildResponseInvalid, wantStage: MCPReadinessStageToolsList, wantSecondary: true, forbiddenRaw: []string{"primary tools failure", closeFailure.Error()}},
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
			if h == nil || h.OK || h.Readiness.FailureID != tc.wantFailureID || h.Readiness.Stage != tc.wantStage {
				t.Fatalf("primary readiness = %+v, want failure=%q stage=%q", h, tc.wantFailureID, tc.wantStage)
			}
			if tc.wantSecondary {
				if h.Readiness.SecondaryFailureID != "MCP_HEALTH_SESSION_CLEANUP_FAILED" || h.Readiness.SecondaryStage != MCPReadinessStageSessionCleanup {
					t.Fatalf("secondary cleanup evidence = %+v", h.Readiness)
				}
			} else if h.Readiness.SecondaryFailureID != "" || h.Readiness.SecondaryStage != "" {
				t.Fatalf("unexpected secondary failure on cleanup-primary result: %+v", h.Readiness)
			}
			for _, forbidden := range tc.forbiddenRaw {
				if strings.Contains(h.Err, forbidden) {
					t.Fatalf("raw cause %q leaked: %q", forbidden, h.Err)
				}
			}
		})
	}
}

// TestSingleHealthProbe_SessionCleanupFailureSettlesAllPostInitializeReturns
// catches a cleanup regression that overwrites a forward failure or leaks a
// raw probe cause after the session-owning initialize response succeeds.
func TestSingleHealthProbe_SessionCleanupFailureSettlesAllPostInitializeReturns(t *testing.T) {
	cleanupResponse := func() (*http.Response, error) {
		return healthProbeResponse(http.StatusBadGateway, io.NopCloser(strings.NewReader("cleanup response sentinel"))), nil
	}
	for _, tc := range []struct {
		name          string
		list          func() (*http.Response, error)
		cleanup       func() (*http.Response, error)
		want          MCPReadinessResult
		wantSecondary bool
		forbiddenRaw  []string
	}{
		{
			name: "transport", list: func() (*http.Response, error) { return nil, errors.New("primary transport sentinel") }, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureReadinessHostUnavailable, Detail: "MCP_READINESS_HOST_UNAVAILABLE at tools_list"},
			wantSecondary: true, forbiddenRaw: []string{"primary transport sentinel"},
		},
		{
			name: "HTTP", list: func() (*http.Response, error) {
				body, err := readinesswire.EncodeFailure(readinesswire.Failure{FailureID: readinesswire.FailureBackingProtocolUnsupported, Stage: readinesswire.StageInitialize, HTTPStatus: http.StatusBadGateway, RequestedProtocol: "2025-03-26", NegotiatedProtocol: "2024-11-05", SupportedFloor: "2025-03-26"})
				if err != nil {
					return nil, err
				}
				resp := healthProbeResponse(http.StatusBadGateway, io.NopCloser(strings.NewReader(string(body))))
				resp.Header.Set("Content-Type", readinesswire.MediaTypeV1)
				return resp, nil
			}, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageInitialize, FailureID: readinesswire.FailureBackingProtocolUnsupported, HTTPStatus: http.StatusBadGateway, RequestedProtocol: "2025-03-26", NegotiatedProtocol: "2024-11-05", SupportedFloor: "2025-03-26", Detail: "MCP_BACKING_PROTOCOL_UNSUPPORTED at initialize (HTTP 502; requested 2025-03-26; negotiated 2024-11-05)"},
			wantSecondary: true,
		},
		{
			name: "body read", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, failingHealthProbeBody{err: errors.New("primary body read sentinel")}), nil
			}, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"},
			wantSecondary: true, forbiddenRaw: []string{"primary body read sentinel"},
		},
		{
			name: "oversized", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(strings.Repeat("primary oversize sentinel", maxHealthProbeResponseBytes/24+1)))), nil
			}, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"},
			wantSecondary: true, forbiddenRaw: []string{"primary oversize sentinel"},
		},
		{
			name: "JSON parse", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader("primary parse sentinel"))), nil
			}, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"},
			wantSecondary: true, forbiddenRaw: []string{"primary parse sentinel"},
		},
		{
			name: "JSON RPC error", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary RPC sentinel"}}`))), nil
			}, cleanup: cleanupResponse,
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"},
			wantSecondary: true, forbiddenRaw: []string{"primary RPC sentinel"},
		},
		{
			name: "valid tools result", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[{}]}}`))), nil
			}, cleanup: cleanupResponse,
			want: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageSessionCleanup, FailureID: MCPReadinessFailureSessionCleanup, Detail: "MCP_HEALTH_SESSION_CLEANUP_FAILED at session_cleanup"},
		},
		{
			name: "cleanup transport", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[]}}`))), nil
			}, cleanup: func() (*http.Response, error) { return nil, errors.New("cleanup transport sentinel") },
			want:         MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageSessionCleanup, FailureID: MCPReadinessFailureSessionCleanup, Detail: "MCP_HEALTH_SESSION_CLEANUP_FAILED at session_cleanup"},
			forbiddenRaw: []string{"cleanup transport sentinel"},
		},
		{
			name: "cleanup drain", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","result":{"tools":[]}}`))), nil
			}, cleanup: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusNoContent, &failingCleanupBody{drainErr: errors.New("cleanup drain sentinel")}), nil
			},
			want:         MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageSessionCleanup, FailureID: MCPReadinessFailureSessionCleanup, Detail: "MCP_HEALTH_SESSION_CLEANUP_FAILED at session_cleanup"},
			forbiddenRaw: []string{"cleanup drain sentinel"},
		},
		{
			name: "cleanup close preserves forward primary", list: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusOK, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"primary close sentinel"}}`))), nil
			}, cleanup: func() (*http.Response, error) {
				return healthProbeResponse(http.StatusNoContent, &failingCleanupBody{closeErr: errors.New("cleanup close sentinel")}), nil
			},
			want:          MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"},
			wantSecondary: true,
			forbiddenRaw:  []string{"primary close sentinel", "cleanup close sentinel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := healthProbeHTTPClient
			t.Cleanup(func() { healthProbeHTTPClient = previous })
			deleteCalls := 0
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
						return tc.list()
					case http.MethodDelete:
						deleteCalls++
						if got := req.Header.Get("Mcp-Session-Id"); got != "issued-session" {
							return nil, errors.New("cleanup session sentinel")
						}
						return tc.cleanup()
					default:
						return nil, fmt.Errorf("unexpected method %s", req.Method)
					}
				})}
			}

			got := singleHealthProbe(1)
			if got == nil || got.OK {
				t.Fatalf("probe = %#v, want unready", got)
			}
			primary := got.Readiness
			primary.SecondaryStage = ""
			primary.SecondaryFailureID = ""
			if primary != tc.want {
				t.Fatalf("primary readiness = %#v, want %#v", primary, tc.want)
			}
			if deleteCalls != 1 {
				t.Fatalf("DELETE calls = %d, want 1", deleteCalls)
			}
			if tc.wantSecondary {
				if got.Readiness.SecondaryStage != MCPReadinessStageSessionCleanup || got.Readiness.SecondaryFailureID != MCPReadinessFailureSessionCleanup {
					t.Fatalf("secondary = %#v", got.Readiness)
				}
			} else if got.Readiness.SecondaryStage != "" || got.Readiness.SecondaryFailureID != "" {
				t.Fatalf("cleanup-primary result has secondary = %#v", got.Readiness)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal health probe: %v", err)
			}
			for _, raw := range append(tc.forbiddenRaw, "issued-session", "cleanup response sentinel", "cleanup session sentinel") {
				if strings.Contains(got.Err, raw) || strings.Contains(got.Readiness.Detail, raw) || strings.Contains(string(encoded), raw) {
					t.Fatalf("raw %q leaked in probe=%#v json=%s", raw, got, encoded)
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
