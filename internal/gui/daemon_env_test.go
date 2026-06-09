package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

func TestDaemonEnvGETListsCurrentIntentWithOverrides(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-memory-default`,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, "daemon-env-overrides.yaml"), func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env: map[string]string{
				"MEMORY_FILE_PATH": `D:\memory\memory.jsonl`,
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	s := NewServer(Config{Port: 9125})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9125/api/daemon/env", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Daemons []struct {
			TaskName string            `json:"task_name"`
			Server   string            `json:"server"`
			Daemon   string            `json:"daemon"`
			Env      map[string]string `json:"env"`
		} `json:"daemons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Daemons) != 1 {
		t.Fatalf("daemons len = %d, want 1: %+v", len(body.Daemons), body)
	}
	got := body.Daemons[0]
	if got.TaskName != `\mcp-local-hub-memory-default` || got.Server != "memory" || got.Daemon != "default" {
		t.Fatalf("daemon row identity = %+v, want memory/default", got)
	}
	if got.Env["MEMORY_FILE_PATH"] != `D:\memory\memory.jsonl` {
		t.Fatalf("MEMORY_FILE_PATH = %q, want D:\\memory\\memory.jsonl", got.Env["MEMORY_FILE_PATH"])
	}
}

func TestDaemonEnvGETExcludesMaintenanceTasks(t *testing.T) {
	// deep-sec #268: the env list endpoint iterates intent rows; weekly-refresh /
	// watchdog maintenance tasks are one-shot scheduler jobs, not daemons with env
	// overrides, and must not appear (or be selectable for Apply/Restart).
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 9123},
			{TaskName: `\mcp-local-hub-memory-weekly-refresh`, Server: "memory", Daemon: "weekly-refresh"},
		},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	s := NewServer(Config{Port: 9125})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9125/api/daemon/env", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Daemons []struct {
			TaskName string `json:"task_name"`
		} `json:"daemons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Daemons) != 1 || body.Daemons[0].TaskName != `\mcp-local-hub-memory-default` {
		t.Fatalf("daemons = %+v, want only memory-default (weekly-refresh excluded)", body.Daemons)
	}
}

func TestDaemonRespawnRejectsMaintenanceTask(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	s := NewServer(Config{Port: 9125})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9125/api/daemon/respawn",
		strings.NewReader(`{"task_name":"\\mcp-local-hub-memory-weekly-refresh"}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)

	// The maintenance guard fires before any IPC dial.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a maintenance respawn; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MAINTENANCE_TASK") {
		t.Fatalf("body = %s, want MAINTENANCE_TASK error code", rec.Body.String())
	}
}

func TestDaemonEnvGETPrefersDescriptorIdentityOverTaskNameParsing(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-lsp-deadbeef-go`,
			Server:   "mcp-language-server",
			Daemon:   "lsp-deadbeef-go",
			Port:     9124,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	s := NewServer(Config{Port: 9125})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9125/api/daemon/env", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Daemons []struct {
			TaskName string `json:"task_name"`
			Server   string `json:"server"`
			Daemon   string `json:"daemon"`
		} `json:"daemons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Daemons) != 1 {
		t.Fatalf("daemons len = %d, want 1: %+v", len(body.Daemons), body)
	}
	got := body.Daemons[0]
	if got.TaskName != `\mcp-local-hub-lsp-deadbeef-go` ||
		got.Server != "mcp-language-server" ||
		got.Daemon != "lsp-deadbeef-go" {
		t.Fatalf("daemon row identity = %+v, want descriptor mcp-language-server/lsp-deadbeef-go", got)
	}
}
