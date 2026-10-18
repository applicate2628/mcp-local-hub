package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/mcpcompat/readinesswire"
)

func TestMCPReadinessHostFailureSurvivesWireProjection(t *testing.T) {
	producer := httptest.NewRecorder()
	if err := readinesswire.WriteEmergencyFailure(producer); err != nil {
		t.Fatal(err)
	}
	response := producer.Result()
	defer response.Body.Close()
	failure := readinesswire.DecodeFailureResponse(response.StatusCode, response.Header.Get("Content-Type"), response.Body)
	if failure.Stage != "host" || failure.FailureID != "MCP_READINESS_HOST_UNAVAILABLE" || failure.HTTPStatus != http.StatusServiceUnavailable || !failure.Retryable {
		t.Fatalf("producer/decoder failure = %#v", failure)
	}
	projected := readinessFromFailure(failure)
	const wantDetail = "MCP_READINESS_HOST_UNAVAILABLE at host (HTTP 503)"
	if projected.State != MCPReadinessUnready || projected.Stage != "host" || projected.FailureID != failure.FailureID || projected.HTTPStatus != 503 || !projected.Retryable || projected.Detail != wantDetail {
		t.Fatalf("API host projection = %#v", projected)
	}
	probe := healthProbeFromReadiness(projected)
	if probe.OK || probe.Err != wantDetail || probe.Readiness != projected {
		t.Fatalf("retained health projection = %#v", probe)
	}
	if (&MCPReadinessError{Result: projected}).Error() != wantDetail {
		t.Fatal("typed compatibility error lost causal host detail")
	}
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	var receiving MCPReadinessResult
	if err := json.Unmarshal(raw, &receiving); err != nil {
		t.Fatal(err)
	}
	if receiving.State != MCPReadinessUnready || receiving.Stage != "host" || receiving.FailureID != "MCP_READINESS_HOST_UNAVAILABLE" || receiving.HTTPStatus != 503 || receiving.Detail != wantDetail {
		t.Fatalf("serialized API receiving result = %#v", receiving)
	}
}
