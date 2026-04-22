// internal/gui/status_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeStatus struct {
	rows []api.DaemonStatus
	err  error
}

func (f fakeStatus) Status() ([]api.DaemonStatus, error) { return f.rows, f.err }

func TestStatus_ReturnsArrayOfDaemonStatus(t *testing.T) {
	s := NewServer(Config{})
	s.status = fakeStatus{rows: []api.DaemonStatus{
		{Server: "memory", TaskName: "mcp-local-hub-memory-default", State: "Running", Port: 9123},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out []api.DaemonStatus
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Server != "memory" || out[0].Port != 9123 {
		t.Errorf("unexpected rows: %+v", out)
	}
}
