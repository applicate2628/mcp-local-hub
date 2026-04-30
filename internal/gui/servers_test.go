// internal/gui/servers_test.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeRestart struct {
	called  string
	results []api.RestartResult
	err     error
}

func (f *fakeRestart) Restart(server string) ([]api.RestartResult, error) {
	f.called = server
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
	if fr.called != "memory" {
		t.Errorf("Restart called with %q, want memory", fr.called)
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
	called  string
	results []api.RestartResult
	err     error
}

func (f *fakeStop) Stop(server string) ([]api.RestartResult, error) {
	f.called = server
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
	if fs.called != "memory" {
		t.Errorf("Stop called with %q, want memory", fs.called)
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
