// internal/gui/status_test.go
package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeStatus implements statusProvider. After Phase 6 (G2) the
// /api/status handler reads via s.health.DaemonStatusSnapshot(), not
// s.status.Status(); fakeStatus is retained because the StatusPoller
// (internal/gui/poller.go) still consumes statusProvider for SSE
// broadcast — keeping the fake in this file means the type stays
// reachable for tests that assert the legacy interface contract.
type fakeStatus struct {
	rows []api.DaemonStatus
	err  error
}

func (f fakeStatus) Status() ([]api.DaemonStatus, error) { return f.rows, f.err }

// TestStatus_ReturnsArrayOfDaemonStatus locks down the /api/status wire
// shape (an array of api.DaemonStatus) regardless of which backend
// path produces the rows. Phase 6 swapped the producer from
// s.status.Status() to s.health.DaemonStatusSnapshot(); the consumer
// contract is unchanged.
func TestStatus_ReturnsArrayOfDaemonStatus(t *testing.T) {
	fake := &fakeHealth{returnDaemonStatuses: []api.DaemonStatus{
		{Server: "memory", TaskName: "mcp-local-hub-memory-default", State: "Running", Port: 9123},
	}}
	s := NewServer(Config{})
	s.health = fake
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

// TestStatusEndpoint_RoutesViaHealthBackend asserts that /api/status
// goes through s.health.DaemonStatusSnapshot() (NOT s.status.Status())
// and that the returned wire shape preserves all DaemonStatus fields
// — including TaskName, NextRun, and other fields that the projected
// DaemonRow shape strips. Phase 6 of G2.
//
// This test is the central guarantee for the Phase 6 refactor: it
// proves the /api/status handler now shares the long-lived health
// cache without losing any fields the existing GUI/CLI consumers
// depend on.
func TestStatusEndpoint_RoutesViaHealthBackend(t *testing.T) {
	fakeStatusOld := fakeStatus{
		// Should NEVER be read by /api/status after Phase 6.
		rows: []api.DaemonStatus{
			{Server: "from-status-provider", State: "Running"},
		},
	}
	fakeH := &fakeHealth{
		returnDaemonStatuses: []api.DaemonStatus{
			{Server: "fs", Daemon: "fs-default",
				TaskName: "mcp-local-hub-fs-default",
				State:    "Running", Port: 9100, PID: 1234,
				RAMBytes: 50_000_000, UptimeSec: 300,
				NextRun: "Sunday, April 19, 2026 3:00:00 AM"},
		},
	}
	s := NewServer(Config{})
	s.status = fakeStatusOld
	s.health = fakeH

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var rows []api.DaemonStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Server != "fs" || rows[0].PID != 1234 || rows[0].State != "Running" {
		t.Errorf("row 0 = %+v, want existing wire shape (state=Running, etc.)", rows[0])
	}
	if rows[0].TaskName != "mcp-local-hub-fs-default" {
		t.Errorf("TaskName lost: %q (the DaemonRow projection drops this field — handler must read DaemonStatusSnapshot, not project from HealthSnapshot.Daemons)", rows[0].TaskName)
	}
	if rows[0].NextRun != "Sunday, April 19, 2026 3:00:00 AM" {
		t.Errorf("NextRun lost: %q", rows[0].NextRun)
	}
	if fakeH.daemonStatusCalls != 1 {
		t.Errorf("expected 1 call to DaemonStatusSnapshot; got %d", fakeH.daemonStatusCalls)
	}
	// Hard-prove the legacy s.status path is dead for /api/status:
	// fakeStatusOld returns Server="from-status-provider", which would
	// surface in rows[0].Server if the handler still called
	// s.status.Status().
	if rows[0].Server == "from-status-provider" {
		t.Errorf("handler still routes through s.status.Status(); must use s.health.DaemonStatusSnapshot()")
	}
}

// TestStatusEndpoint_500OnHealthBackendError verifies error mapping:
// a DaemonStatusSnapshot fetch failure must surface as 500 with the
// STATUS_FAILED error code (the existing wire-shape guard).
func TestStatusEndpoint_500OnHealthBackendError(t *testing.T) {
	fakeH := &fakeHealth{returnDaemonErr: errors.New("snapshot exploded")}
	s := NewServer(Config{})
	s.health = fakeH

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s; want 500", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "STATUS_FAILED") {
		t.Errorf("body = %s, want STATUS_FAILED code", body)
	}
}

// TestStatusEndpoint_ExposesSupervisorDownDegradedMarker is the v0.6
// Workstream B (§3.1) GUI-layer assertion: when the supervisor is
// unreachable the daemons-section fetch returns api.ErrSupervisorDown
// instead of falling back to stale scheduler rows, and the /api/status
// route must surface that as 500 STATUS_FAILED with the operator-facing
// message in the body. The frontend Dashboard turns this into the
// "Failed to load status — Restart supervisor" recovery surface, so the
// message must NAME the action ("restart the hub"), not be a bare code.
func TestStatusEndpoint_ExposesSupervisorDownDegradedMarker(t *testing.T) {
	fakeH := &fakeHealth{returnDaemonErr: api.ErrSupervisorDown}
	s := NewServer(Config{})
	s.health = fakeH

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s; want 500", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != "STATUS_FAILED" {
		t.Errorf("code = %q, want STATUS_FAILED", body.Code)
	}
	if !strings.Contains(body.Error, "restart the hub") {
		t.Errorf("error = %q, want it to name the operator action (restart the hub)", body.Error)
	}
}

// TestStatusEndpoint_RedactsSetupFailurePath is the phase-1 finding-4 companion to
// the ErrSupervisorDown allowlist above: a NON-ErrSupervisorDown status error (here a
// DialSupervisorIPCStatus setup failure that wraps the absolute state-dir path via
// ErrStatusSetupFailure) MUST be redacted — the response body carries the opaque
// "internal error", never the absolute path, while keeping the STATUS_FAILED code.
func TestStatusEndpoint_RedactsSetupFailurePath(t *testing.T) {
	const leakyPath = `/home/alice/.local/state/mcp-local-hub`
	leaky := fmt.Errorf("supervisor IPC status: resolve state dir: %s: owner UID 1000 != current UID 1001: %w",
		leakyPath, api.ErrStatusSetupFailure)
	fakeH := &fakeHealth{returnDaemonErr: leaky}
	s := NewServer(Config{})
	s.health = fakeH

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s; want 500", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != "STATUS_FAILED" {
		t.Errorf("code = %q, want STATUS_FAILED", body.Code)
	}
	if body.Error != "internal error" {
		t.Errorf("error = %q, want redacted \"internal error\"", body.Error)
	}
	if strings.Contains(rec.Body.String(), leakyPath) {
		t.Errorf("response body leaks the absolute state-dir path %q: %s", leakyPath, rec.Body.String())
	}
}

func TestGUIStatusUsesIPCSeamWhenWired(t *testing.T) {
	prev := api.SupervisorIPCStatusFn
	api.SupervisorIPCStatusFn = func(_ context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{
				Server:   "memory",
				Daemon:   "default",
				TaskName: `\mcp-local-hub-memory-default`,
				State:    "Running",
				Port:     9101,
				PID:      4321,
			},
		}, nil
	}
	t.Cleanup(func() { api.SupervisorIPCStatusFn = prev })

	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var rows []api.DaemonStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].TaskName != `\mcp-local-hub-memory-default` || rows[0].PID != 4321 {
		t.Fatalf("/api/status did not use wired supervisor IPC seam: %+v", rows[0])
	}
}
