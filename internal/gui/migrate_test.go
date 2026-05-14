// internal/gui/migrate_test.go
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

type fakeMigrator struct {
	calledServers []string
	calledClients []string
	report        *api.MigrateReport
	err           error
}

func (f *fakeMigrator) Migrate(servers, clients []string) (*api.MigrateReport, error) {
	f.calledServers = servers
	f.calledClients = clients
	return f.report, f.err
}

// TestMigrate_CallsAPIWithServerList pins the happy-path: full success
// emits 200 + structured MigrateReport with empty Failed[] (B1 #7
// symmetry with demigrate replaces the previous 204 + empty body).
func TestMigrate_CallsAPIWithServerList(t *testing.T) {
	fm := &fakeMigrator{
		report: &api.MigrateReport{
			Applied: []api.AppliedMigration{
				{Server: "memory", Client: "claude-code", URL: "http://127.0.0.1:9128/mcp"},
				{Server: "wolfram", Client: "claude-code", URL: "http://127.0.0.1:9129/mcp"},
			},
		},
	}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory","wolfram"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.calledServers, ",") != "memory,wolfram" {
		t.Errorf("Migrate called with servers=%v", fm.calledServers)
	}
	if len(fm.calledClients) != 0 {
		t.Errorf("Migrate called with clients=%v, want empty", fm.calledClients)
	}
	var resp api.MigrateReport
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Applied) != 2 {
		t.Errorf("applied=%+v, want 2 rows", resp.Applied)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed=%+v, want empty", resp.Failed)
	}
}

// TestMigrate_ForwardsClientsSubset guards the per-cell Apply path: when the
// GUI toggles a single (server, client) cell, the request body carries both
// a servers list and a clients list, and the handler must forward clients
// into the migrator so ClientsInclude narrows the rewrite — otherwise one
// flipped checkbox silently rewrites every client binding for that server.
func TestMigrate_ForwardsClientsSubset(t *testing.T) {
	fm := &fakeMigrator{
		report: &api.MigrateReport{
			Applied: []api.AppliedMigration{{Server: "memory", Client: "claude-code", URL: "http://127.0.0.1:9128/mcp"}},
		},
	}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory"],"clients":["claude-code"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.calledServers, ",") != "memory" {
		t.Errorf("Migrate called with servers=%v", fm.calledServers)
	}
	if strings.Join(fm.calledClients, ",") != "claude-code" {
		t.Errorf("Migrate called with clients=%v, want [claude-code]", fm.calledClients)
	}
}

// TestMigrate_PartialFailureReturns207 pins B1 #7 closure symmetry with
// /api/demigrate: per-row failures surface via 207 Multi-Status with
// structured body, NOT a flattened 500 error blob.
func TestMigrate_PartialFailureReturns207(t *testing.T) {
	fm := &fakeMigrator{
		report: &api.MigrateReport{
			Applied: []api.AppliedMigration{
				{Server: "memory", Client: "claude-code", URL: "http://127.0.0.1:9128/mcp"},
			},
			Failed: []api.FailedMigration{
				{Server: "wolfram", Client: "gemini-cli", Err: "open settings.json: file not found"},
			},
		},
	}
	s := NewServer(Config{})
	s.migrator = fm

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory","wolfram"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("want 207 Multi-Status, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp api.MigrateReport
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Failed) != 1 {
		t.Fatalf("failed=%+v, want 1 row", resp.Failed)
	}
	if strings.Contains(resp.Failed[0].Err, "migration row(s) failed") {
		t.Errorf("failed[0].err contains aggregation prefix (B1 regression): %q", resp.Failed[0].Err)
	}
}

func TestMigrate_BadMethodReturns405(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/migrate", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}
