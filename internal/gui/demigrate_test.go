package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeDemigrater struct {
	gotServers []string
	gotClients []string
	report     *api.DemigrateReport
	err        error
}

func (f *fakeDemigrater) Demigrate(servers, clients []string) (*api.DemigrateReport, error) {
	f.gotServers = append([]string{}, servers...)
	f.gotClients = append([]string{}, clients...)
	return f.report, f.err
}

func TestDemigrateHandler_RejectsNonPOST(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	req := httptest.NewRequest(http.MethodGet, "/api/demigrate", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDemigrateHandler_ForwardsServersAndClients pins the happy-path:
// successful rollback emits 200 + structured DemigrateReport with empty
// Failed[] (B1 #7 closure replaces the previous 204 + empty body).
func TestDemigrateHandler_ForwardsServersAndClients(t *testing.T) {
	fake := &fakeDemigrater{
		report: &api.DemigrateReport{
			Restored: []api.RestoredMigration{{Server: "memory", Client: "claude-code"}},
		},
	}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"],"clients":["claude-code"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", got)
	}
	var resp api.DemigrateReport
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Restored) != 1 || resp.Restored[0].Server != "memory" {
		t.Errorf("restored=%+v, want [{memory claude-code}]", resp.Restored)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed=%+v, want empty", resp.Failed)
	}
	if len(fake.gotServers) != 1 || fake.gotServers[0] != "memory" {
		t.Errorf("gotServers=%v, want [memory]", fake.gotServers)
	}
	if len(fake.gotClients) != 1 || fake.gotClients[0] != "claude-code" {
		t.Errorf("gotClients=%v, want [claude-code]", fake.gotClients)
	}
}

// TestDemigrateHandler_PartialFailureReturns207 pins B1 #7 closure: per-
// row failures surface via 207 Multi-Status with structured body. The
// pre-B1 behavior aggregated all failures into a single 500 error blob
// like `1 demigrate row(s) failed: server/client: sentinel ...`.
func TestDemigrateHandler_PartialFailureReturns207(t *testing.T) {
	fake := &fakeDemigrater{
		report: &api.DemigrateReport{
			Restored: []api.RestoredMigration{{Server: "memory", Client: "claude-code"}},
			Failed: []api.FailedMigration{
				{Server: "memory", Client: "gemini-cli", Err: "sentinel C:\\Users\\u\\.gemini\\settings.json.bak-mcp-local-hub-original unreadable: open ...: file not found"},
				{Server: "time", Client: "gemini-cli", Err: "no backup found (migration may never have run on this machine)"},
			},
		},
	}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory","time"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("want 207 Multi-Status, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", got)
	}
	var resp api.DemigrateReport
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Restored) != 1 {
		t.Errorf("restored=%+v, want 1 row", resp.Restored)
	}
	if len(resp.Failed) != 2 {
		t.Fatalf("failed=%+v, want 2 rows", resp.Failed)
	}
	if resp.Failed[0].Server != "memory" || resp.Failed[0].Client != "gemini-cli" {
		t.Errorf("failed[0]=%+v", resp.Failed[0])
	}
	if !strings.Contains(resp.Failed[0].Err, "sentinel") {
		t.Errorf("failed[0].err should preserve underlying message verbatim; got %q", resp.Failed[0].Err)
	}
	// Crucially: no aggregation prefix like "N demigrate row(s) failed:"
	// — that was the B1 wall-of-text source.
	if strings.Contains(resp.Failed[0].Err, "demigrate row(s) failed") {
		t.Errorf("failed[0].err contains aggregation prefix (B1 regression): %q", resp.Failed[0].Err)
	}
}

// TestDemigrateHandler_NilReportTreatedAsEmpty defends against a
// realDemigrater returning (nil, nil) — defensive guard, should never
// happen but failing loudly via Encode(nil) would corrupt the frontend.
func TestDemigrateHandler_NilReportTreatedAsEmpty(t *testing.T) {
	fake := &fakeDemigrater{} // report=nil, err=nil
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.DemigrateReport
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Restored) != 0 || len(resp.Failed) != 0 {
		t.Errorf("nil report should encode as empty {restored:null, failed:null}; got %+v", resp)
	}
}

func TestDemigrateHandler_SurfacesDemigrateError(t *testing.T) {
	fake := &fakeDemigrater{err: errStub("boom")}
	s := NewServer(Config{Port: 0})
	s.demigrater = fake
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var body2 struct{ Error, Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body2)
	if !strings.Contains(body2.Error, "boom") {
		t.Errorf("error=%q, want contains boom", body2.Error)
	}
	if body2.Code != "DEMIGRATE_FAILED" {
		t.Errorf("code=%q, want DEMIGRATE_FAILED", body2.Code)
	}
}

func TestDemigrateHandler_RejectsCrossOrigin(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.demigrater = &fakeDemigrater{}
	body := bytes.NewReader([]byte(`{"servers":["memory"]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/demigrate", body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
