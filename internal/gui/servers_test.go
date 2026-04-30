// internal/gui/servers_test.go
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type fakeRestart struct {
	calledServer     string
	calledDaemon     string
	calledRestartAll bool
	results          []api.RestartResult
	err              error
}

func (f *fakeRestart) Restart(server, daemon string) ([]api.RestartResult, error) {
	f.calledServer = server
	f.calledDaemon = daemon
	return f.results, f.err
}

func (f *fakeRestart) RestartAll() ([]api.RestartResult, error) {
	f.calledRestartAll = true
	return f.results, f.err
}

func TestRestartServer_InvokesAPI(t *testing.T) {
	fr := &fakeRestart{}
	s := NewServer(Config{})
	s.restart = fr
	req := httptest.NewRequest(http.MethodPost, "/api/servers/memory/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if fr.calledServer != "memory" {
		t.Errorf("Restart called with server=%q, want memory", fr.calledServer)
	}
	if fr.calledDaemon != "" {
		t.Errorf("Restart called with daemon=%q, want empty (no ?daemon)", fr.calledDaemon)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if _, ok := got["restart_results"]; !ok {
		t.Errorf("body missing restart_results: %v", got)
	}
}

func TestRestart_PartialFailureReturns207(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-server-a-default", Err: ""},
			{TaskName: "mcp-local-hub-server-b-default", Err: "scheduler timeout"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		RestartResults []api.RestartResult `json:"restart_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.RestartResults) != 2 || body.RestartResults[1].Err != "scheduler timeout" {
		t.Errorf("results = %+v", body.RestartResults)
	}
}

func TestRestart_OrchestrationFailureReturns500(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{{TaskName: "x", Err: ""}},
		err:     fmt.Errorf("scheduler unavailable"),
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body struct {
		RestartResults []api.RestartResult `json:"restart_results"`
		Error          string              `json:"error"`
		Code           string              `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "RESTART_FAILED" {
		t.Errorf("code=%q", body.Code)
	}
	if len(body.RestartResults) != 1 {
		t.Errorf("partial results dropped: %+v", body.RestartResults)
	}
}

type fakeStop struct {
	calledServer  string
	calledDaemon  string
	calledStopAll bool
	results       []api.RestartResult
	err           error
}

func (f *fakeStop) Stop(server, daemon string) ([]api.RestartResult, error) {
	f.calledServer = server
	f.calledDaemon = daemon
	return f.results, f.err
}

func (f *fakeStop) StopAll() ([]api.RestartResult, error) {
	f.calledStopAll = true
	return f.results, f.err
}

func TestStopServer_InvokesAPI(t *testing.T) {
	fs := &fakeStop{}
	s := NewServer(Config{})
	s.stop = fs
	req := httptest.NewRequest(http.MethodPost, "/api/servers/memory/stop", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if fs.calledServer != "memory" {
		t.Errorf("Stop called with server=%q, want memory", fs.calledServer)
	}
	if fs.calledDaemon != "" {
		t.Errorf("Stop called with daemon=%q, want empty (no ?daemon)", fs.calledDaemon)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if _, ok := got["stop_results"]; !ok {
		t.Errorf("body missing stop_results: %v", got)
	}
}

func TestStop_PartialFailureReturns207(t *testing.T) {
	fs := &fakeStop{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-server-a-default", Err: ""},
			{TaskName: "mcp-local-hub-server-b-default", Err: "kill timeout"},
		},
	}
	s := NewServer(Config{})
	s.stop = fs

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/stop", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		StopResults []api.RestartResult `json:"stop_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.StopResults) != 2 || body.StopResults[1].Err != "kill timeout" {
		t.Errorf("results = %+v", body.StopResults)
	}
}

func TestStop_OrchestrationFailureReturns500(t *testing.T) {
	fs := &fakeStop{
		results: []api.RestartResult{{TaskName: "x", Err: ""}},
		err:     fmt.Errorf("scheduler unavailable"),
	}
	s := NewServer(Config{})
	s.stop = fs

	req := httptest.NewRequest(http.MethodPost, "/api/servers/server-a/stop", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body struct {
		StopResults []api.RestartResult `json:"stop_results"`
		Error       string              `json:"error"`
		Code        string              `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "STOP_FAILED" {
		t.Errorf("code=%q", body.Code)
	}
	if len(body.StopResults) != 1 {
		t.Errorf("partial results dropped: %+v", body.StopResults)
	}
}

func TestStop_MethodNotAllowed(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/servers/memory/stop", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

// --- ?daemon= query parameter (Codex CLI consult matrix, 2026-04-30) ---

func TestRestart_DaemonQueryNarrowsToOneTask(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-serena-codex"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/restart?daemon=codex", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if fr.calledServer != "serena" {
		t.Errorf("server=%q, want serena", fr.calledServer)
	}
	if fr.calledDaemon != "codex" {
		t.Errorf("daemon=%q, want codex — backend MUST receive the filter", fr.calledDaemon)
	}
}

func TestRestart_EmptyDaemonQueryRejected(t *testing.T) {
	s := NewServer(Config{})
	s.restart = &fakeRestart{} // would silently succeed if not rejected upstream
	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/restart?daemon=", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (empty daemon must not silently mean 'all')", rec.Code)
	}
}

func TestRestart_RepeatedDaemonQueryRejected(t *testing.T) {
	s := NewServer(Config{})
	s.restart = &fakeRestart{}
	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/restart?daemon=claude&daemon=codex", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (multiple daemon values not supported)", rec.Code)
	}
}

func TestRestart_UnknownDaemonReturns404(t *testing.T) {
	// api.Restart with an unknown daemonFilter returns an empty results
	// slice and nil error — there are no matching tasks. The handler
	// must convert that to 404 instead of "Restarted" no-op.
	fr := &fakeRestart{results: []api.RestartResult{}}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/restart?daemon=ghost", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (unknown daemon must surface as error, not silent no-op)", rec.Code)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "DAEMON_NOT_FOUND" {
		t.Errorf("code=%q, want DAEMON_NOT_FOUND", body.Code)
	}
}

func TestStop_DaemonQueryNarrowsToOneTask(t *testing.T) {
	fs := &fakeStop{
		results: []api.RestartResult{{TaskName: "mcp-local-hub-serena-codex"}},
	}
	s := NewServer(Config{})
	s.stop = fs

	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/stop?daemon=codex", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if fs.calledDaemon != "codex" {
		t.Errorf("daemon=%q, want codex", fs.calledDaemon)
	}
}

func TestStop_UnknownDaemonReturns404(t *testing.T) {
	fs := &fakeStop{results: []api.RestartResult{}}
	s := NewServer(Config{})
	s.stop = fs

	req := httptest.NewRequest(http.MethodPost, "/api/servers/serena/stop?daemon=ghost", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestRestart_NoDaemonQueryEmptyResultsStill200(t *testing.T) {
	// A server with no scheduler tasks at all (newly-installed, never
	// scheduled) returns empty results with nil error from api.Restart.
	// Without ?daemon=, that is normal "no daemons to restart" — must
	// stay 200, not 404. The 404 conversion only applies when
	// ?daemon=<name> was given (= filter targeted nothing).
	fr := &fakeRestart{results: []api.RestartResult{}}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/servers/empty/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (empty results without ?daemon is normal)", rec.Code)
	}
}

// --- Bulk action routes (Run all / Stop all on Dashboard, Quit-and-stop in tray) ---

func TestRestartAll_DispatchesBulkRoute(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-memory-default"},
			{TaskName: "mcp-local-hub-time-default"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/restart-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if !fr.calledRestartAll {
		t.Error("RestartAll was not invoked — bulk route did not dispatch to s.restart.RestartAll")
	}
	var body struct {
		RestartResults []api.RestartResult `json:"restart_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.RestartResults) != 2 {
		t.Errorf("expected 2 results, got %+v", body.RestartResults)
	}
}

func TestStopAll_DispatchesBulkRoute(t *testing.T) {
	fs := &fakeStop{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-memory-default"},
			{TaskName: "mcp-local-hub-time-default"},
		},
	}
	s := NewServer(Config{})
	s.stop = fs

	req := httptest.NewRequest(http.MethodPost, "/api/stop-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	if !fs.calledStopAll {
		t.Error("StopAll was not invoked — bulk route did not dispatch to s.stop.StopAll")
	}
}

func TestRestartAll_PartialFailureReturns207(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-memory-default"},
			{TaskName: "mcp-local-hub-time-default", Err: "kill timeout"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	req := httptest.NewRequest(http.MethodPost, "/api/restart-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207 (partial failure)", rec.Code)
	}
}

func TestStopAll_MethodNotAllowed(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/stop-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
}

// PR #38 unified pipeline: bulk-action handlers publish SSE lifecycle
// events on the existing Broadcaster so any open Dashboard sees the
// action's progress regardless of trigger source. Without this, tray-
// triggered fan-outs would be invisible to the Dashboard's bulk-action
// UI state.
func TestRestartAll_PublishesSseLifecycleEvents(t *testing.T) {
	fr := &fakeRestart{results: []api.RestartResult{{TaskName: "mcp-local-hub-memory-default"}}}
	s := NewServer(Config{})
	s.restart = fr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/restart-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var got []Event
	for len(got) < 2 {
		select {
		case ev := <-sub:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for SSE events; got %d: %+v", len(got), got)
		}
	}
	if got[0].Type != "bulk-action" || got[0].Body["phase"] != "started" || got[0].Body["action"] != "restart" {
		t.Errorf("event[0] = %+v, want {bulk-action started restart}", got[0])
	}
	if got[1].Type != "bulk-action" || got[1].Body["phase"] != "completed" || got[1].Body["action"] != "restart" {
		t.Errorf("event[1] = %+v, want {bulk-action completed restart}", got[1])
	}
}

func TestStopAll_PublishesSseLifecycleEvents(t *testing.T) {
	fs := &fakeStop{results: []api.RestartResult{{TaskName: "mcp-local-hub-memory-default"}}}
	s := NewServer(Config{})
	s.stop = fs

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/stop-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var got []Event
	for len(got) < 2 {
		select {
		case ev := <-sub:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out: %d events", len(got))
		}
	}
	if got[1].Body["phase"] != "completed" || got[1].Body["action"] != "stop" {
		t.Errorf("event[1] = %+v, want completed/stop", got[1])
	}
}

// Codex bot PR #38 P1: partial failures (any per-task Err non-empty,
// HTTP 207) must publish SSE phase "error", not "completed". The
// frontend derives the bulk button outcome solely from the SSE phase,
// so if 207 says "completed" the button flashes "Started"/"Stopped"
// on partial failure — wrong.
func TestRestartAll_PartialFailurePublishesErrorPhase(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{
			{TaskName: "mcp-local-hub-memory-default"},
			{TaskName: "mcp-local-hub-time-default", Err: "kill timeout"},
		},
	}
	s := NewServer(Config{})
	s.restart = fr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/restart-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("HTTP=%d, want 207 partial", rec.Code)
	}
	var got []Event
	for len(got) < 2 {
		select {
		case ev := <-sub:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out: %d events", len(got))
		}
	}
	// Lifecycle: started → error (because per-task Err non-empty).
	if got[1].Body["phase"] != "error" {
		t.Errorf("event[1].phase = %v, want error (any per-task Err triggers error phase)", got[1].Body["phase"])
	}
}

// Failure path: when the underlying api errors, the second event must
// be {phase: "error"} so the frontend flashes "Failed".
func TestRestartAll_FailurePublishesErrorPhase(t *testing.T) {
	fr := &fakeRestart{
		results: []api.RestartResult{},
		err:     fmt.Errorf("scheduler unavailable"),
	}
	s := NewServer(Config{})
	s.restart = fr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/restart-all", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	var got []Event
	for len(got) < 2 {
		select {
		case ev := <-sub:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out")
		}
	}
	if got[1].Body["phase"] != "error" {
		t.Errorf("event[1].phase = %v, want error", got[1].Body["phase"])
	}
}
