package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/mcpcompat/readinesswire"
)

// TestMCPReadinessSecondaryFailureWireIsAdditive catches a regression that
// emits one half of the secondary pair or changes ordinary V1 byte shapes.
func TestMCPReadinessSecondaryFailureWireIsAdditive(t *testing.T) {
	for _, tc := range []struct {
		name          string
		result        MCPReadinessResult
		wantSecondary bool
	}{
		{name: "ready", result: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessReady, Stage: MCPReadinessStageToolsList, ToolCount: 1}},
		{name: "primary only", result: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"}},
		{name: "dual failure", result: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageToolsList, FailureID: readinesswire.FailureChildResponseInvalid, SecondaryStage: MCPReadinessStageSessionCleanup, SecondaryFailureID: MCPReadinessFailureSessionCleanup, Detail: "MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"}, wantSecondary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.result)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatalf("decode field map: %v", err)
			}
			_, hasStage := fields["secondary_stage"]
			_, hasFailureID := fields["secondary_failure_id"]
			if hasStage != tc.wantSecondary || hasFailureID != tc.wantSecondary {
				t.Fatalf("secondary fields = stage:%t failure_id:%t in %s", hasStage, hasFailureID, body)
			}
			var decoded MCPReadinessResult
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("round-trip decode: %v", err)
			}
			if decoded != tc.result {
				t.Fatalf("round-trip = %#v, want %#v", decoded, tc.result)
			}
		})
	}

	legacy := []byte(`{"schema_version":"mcp-readiness-v1","state":"unready","stage":"tools_list","failure_id":"MCP_COMPAT_CHILD_RESPONSE_INVALID","http_status":0,"requested_protocol":"","negotiated_protocol":"","supported_floor":"","profile_id":"","tool_count":0,"detail":"MCP_COMPAT_CHILD_RESPONSE_INVALID at tools_list"}`)
	var decoded MCPReadinessResult
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if decoded.SecondaryStage != "" || decoded.SecondaryFailureID != "" {
		t.Fatalf("legacy decode introduced secondary fields: %#v", decoded)
	}
}

func TestSingleHealthProbeProjectsTypedReadinessFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := readinesswire.WriteFailure(w, readinesswire.Failure{
			FailureID:          readinesswire.FailureBackingProtocolUnsupported,
			Stage:              readinesswire.StageInitialize,
			HTTPStatus:         http.StatusBadGateway,
			ManifestName:       "wolfram",
			DaemonName:         "default",
			RequestedProtocol:  "2025-03-26",
			NegotiatedProtocol: "2024-11-05",
			SupportedFloor:     "2025-03-26",
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()

	probe := singleHealthProbe(parsePort(t, srv.URL))
	if probe.OK {
		t.Fatalf("probe unexpectedly OK: %#v", probe)
	}
	want := MCPReadinessResult{
		SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready,
		Stage:             MCPReadinessStageInitialize,
		FailureID:         readinesswire.FailureBackingProtocolUnsupported,
		HTTPStatus:        http.StatusBadGateway,
		RequestedProtocol: "2025-03-26", NegotiatedProtocol: "2024-11-05",
		SupportedFloor: "2025-03-26",
		Detail:         "MCP_BACKING_PROTOCOL_UNSUPPORTED at initialize (HTTP 502; requested 2025-03-26; negotiated 2024-11-05)",
	}
	if probe.Readiness != want {
		t.Fatalf("readiness = %#v, want %#v", probe.Readiness, want)
	}
	if !strings.Contains(probe.Err, readinesswire.FailureBackingProtocolUnsupported) || strings.Contains(probe.Err, "raw-secret") {
		t.Fatalf("legacy projection is not closed/redacted: %q", probe.Err)
	}
}

func TestSingleHealthProbeProjectsExactLegacyFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("initialize negotiated unsupported protocol version \"2024-11-05\"\n"))
	}))
	defer srv.Close()
	probe := singleHealthProbe(parsePort(t, srv.URL))
	if probe.Readiness.FailureID != readinesswire.FailureBackingProtocolUnsupported || probe.Readiness.NegotiatedProtocol != "2024-11-05" {
		t.Fatalf("legacy readiness = %#v", probe.Readiness)
	}
}

func TestMCPReadinessProjectionKeepsProcessStateOrthogonal(t *testing.T) {
	row := DaemonStatus{State: "Running", MCPReadiness: NewMCPReadinessNotChecked()}
	if row.State != "Running" || row.MCPReadiness.State != MCPReadinessNotChecked {
		t.Fatalf("running/not-checked is not representable: %#v", row)
	}
	row.MCPReadiness = MCPReadinessResult{
		SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready,
		Stage: MCPReadinessStageNone, FailureID: readinesswire.FailureReadinessHostUnavailable,
		HTTPStatus: http.StatusServiceUnavailable,
	}
	if row.State != "Running" || row.MCPReadiness.State != MCPReadinessUnready {
		t.Fatalf("running/unready changed process state: %#v", row)
	}
}

func TestInitializeCapabilitySessionPreservesTypedReadinessFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := readinesswire.WriteFailure(w, readinesswire.Failure{
			FailureID:          readinesswire.FailureBackingProtocolUnsupported,
			Stage:              readinesswire.StageInitialize,
			HTTPStatus:         http.StatusBadGateway,
			RequestedProtocol:  "2025-03-26",
			NegotiatedProtocol: "2024-11-05",
			SupportedFloor:     "2025-03-26",
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()

	_, _, _, err := NewAPI().initializeCapabilitySession(DaemonStatus{Port: parsePort(t, srv.URL)})
	var readinessErr *MCPReadinessError
	if !errors.As(err, &readinessErr) {
		t.Fatalf("error = %T %v, want *MCPReadinessError", err, err)
	}
	if readinessErr.Result.FailureID != readinesswire.FailureBackingProtocolUnsupported || readinessErr.Result.NegotiatedProtocol != "2024-11-05" {
		t.Fatalf("typed result = %#v", readinessErr.Result)
	}
	if strings.Contains(err.Error(), "initialize negotiated unsupported") {
		t.Fatalf("raw body leaked through error: %q", err)
	}
}

func TestNormalizeDaemonReadinessSetsBareAndMaintenanceTruth(t *testing.T) {
	rows := []DaemonStatus{
		{State: "Running", Port: 9123},
		{State: "Ready", IsMaintenance: true},
		{State: "Running", Port: 9124, Health: &HealthProbe{Readiness: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessReady, Stage: MCPReadinessStageToolsList, ToolCount: 2}}},
		{State: "Running", Port: 9125, MCPReadiness: MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageInitialize, FailureID: readinesswire.FailureBackingProtocolUnsupported}},
	}
	normalizeDaemonReadiness(rows)
	if rows[0].MCPReadiness.State != MCPReadinessNotChecked {
		t.Fatalf("bare daemon = %#v", rows[0].MCPReadiness)
	}
	if rows[1].MCPReadiness.State != MCPReadinessNotApplicable {
		t.Fatalf("maintenance row = %#v", rows[1].MCPReadiness)
	}
	if rows[2].MCPReadiness.State != MCPReadinessReady || rows[2].MCPReadiness.ToolCount != 2 {
		t.Fatalf("probed daemon = %#v", rows[2].MCPReadiness)
	}
	if rows[3].MCPReadiness.State != MCPReadinessUnready || rows[3].MCPReadiness.FailureID != readinesswire.FailureBackingProtocolUnsupported {
		t.Fatalf("typed IPC readiness was overwritten: %#v", rows[3].MCPReadiness)
	}
}

func TestProbeMCPReadinessIsTheTypedPublicProjection(t *testing.T) {
	previous := healthProbeHTTPClient
	defer func() { healthProbeHTTPClient = previous }()
	healthProbeHTTPClient = func() *http.Client {
		return &http.Client{Transport: healthProbeRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("raw transport detail")
		})}
	}
	result := ProbeMCPReadiness(1)
	if result.State != MCPReadinessUnready || result.FailureID != readinesswire.FailureReadinessHostUnavailable {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.Detail, "raw transport detail") {
		t.Fatalf("raw transport detail leaked: %q", result.Detail)
	}
}
