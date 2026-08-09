// internal/gui/migrate_test.go
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	s := newEphemeralServer(t, Config{})
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

// TestMigrate_EmitsGUIEventOnSuccess covers the deep-review P3
// observability finding: a successful /api/migrate mutation previously
// left no row in gui-events.log at all. The handler must now publish an
// "operator-action" event (SSE-visible immediately, and persisted to
// gui-events.log via the same Broadcaster path bulk-action already
// uses) carrying the server/client identifiers — non-sensitive, no
// credential material is ever in scope for this route.
func TestMigrate_EmitsGUIEventOnSuccess(t *testing.T) {
	fm := &fakeMigrator{
		report: &api.MigrateReport{
			Applied: []api.AppliedMigration{
				{Server: "memory", Client: "claude-code", URL: "http://127.0.0.1:9128/mcp"},
			},
		},
	}
	s := newEphemeralServer(t, Config{})
	s.migrator = fm

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Type != "operator-action" {
			t.Fatalf("event type = %q, want operator-action", ev.Type)
		}
		if ev.Body["action"] != "migrate" {
			t.Errorf("action = %v, want migrate", ev.Body["action"])
		}
		if _, ok := ev.Body["actor"]; !ok {
			t.Errorf("body missing actor field: %+v", ev.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no operator-action event published after a successful migrate")
	}
}

// TestMigrate_NoGUIEventOnFullFailure guards the "emit only after the
// mutation committed" contract: when every row failed (Applied is
// empty), no operator-action event should fire — a failed op must not
// leave a misleading success record in gui-events.log.
func TestMigrate_NoGUIEventOnFullFailure(t *testing.T) {
	fm := &fakeMigrator{
		report: &api.MigrateReport{
			Failed: []api.FailedMigration{
				{Server: "memory", Client: "claude-code", Err: "boom"},
			},
		},
	}
	s := newEphemeralServer(t, Config{})
	s.migrator = fm

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/migrate",
		bytes.NewReader([]byte(`{"servers":["memory"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207, body=%q", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected event published on full-failure migrate: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected: no event
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
	s := newEphemeralServer(t, Config{})
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
	s := newEphemeralServer(t, Config{})
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
	s := newEphemeralServer(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/migrate", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}
