// internal/gui/scan_test.go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeScanner struct {
	result *api.ScanResult
	err    error
}

func (f fakeScanner) Scan() (*api.ScanResult, error) { return f.result, f.err }

func TestScan_ReturnsJSONWrappingAPIResult(t *testing.T) {
	r := &api.ScanResult{}
	s := NewServer(Config{})
	s.scanner = fakeScanner{result: r}
	req := httptest.NewRequest(http.MethodGet, "/api/scan", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// At minimum, scan result should serialize to a JSON object.
	// Exact keys depend on api.ScanResult shape; assert that we got
	// SOMETHING (not "null").
	if out == nil {
		t.Errorf("response decoded to nil map")
	}
}
