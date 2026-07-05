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

func postDaemonEnvRaw(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9125"+path, strings.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, req)
	return rec
}

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

func TestDaemonEnvBodyLimit_OversizedEnvPostRejected(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	body := `{"task_name":"\\mcp-local-hub-memory-default","env":{"PATH":"` + strings.Repeat("A", int(maxControlBodyBytes)+1) + `"}}`
	rec := postDaemonEnvRaw(t, s, "/api/daemon/env", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("body = %s, want shared helper body-too-large error", rec.Body.String())
	}
}

func TestDiscoveryRefreshBodyLimit_TrailingGarbageRejected(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	body := `{}` + strings.Repeat("Z", int(maxControlBodyBytes)+1)
	rec := postDaemonEnvRaw(t, s, "/api/discovery/refresh", body)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body") {
		t.Fatalf("body = %s, want shared helper request-body error", rec.Body.String())
	}
}

func TestDiscoveryRefreshBodyLimit_EmptyBodyStillAccepted(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	rec := postDaemonEnvRaw(t, s, "/api/discovery/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for empty discovery refresh body; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDaemonRespawnBodyLimit_TrailingGarbageRejected(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	body := `{"task_name":"\\mcp-local-hub-memory-weekly-refresh"}` + strings.Repeat("Z", int(maxControlBodyBytes)+1)
	rec := postDaemonEnvRaw(t, s, "/api/daemon/respawn", body)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body") {
		t.Fatalf("body = %s, want shared helper request-body error", rec.Body.String())
	}
}

// TestDaemonRespawnNoSupervisorReturns503 pins the P4 deep-review 503
// mapping at the handler/HTTP layer: with no supervisor.lock.owner.json on
// disk at all, api.DialSupervisorIPCRespawn returns a classified
// RespawnResult{Code: "SUPERVISOR_UNAVAILABLE"} (dialErr == nil), which the
// handler's switch already mapped to 503 before this fix. The companion
// transport-failure case — dialErr != nil from a genuine dial/handshake
// failure (e.g. an owner file present but a dead/unreachable IPC listener)
// — is the path the fix actually changed (500 -> 503); that contract is
// pinned at the transport-client level in
// internal/api/supervisor_ipc_respawn_client_test.go
// (TestDialSupervisorIPCRespawn_HandshakeMismatchReturnsTransportError)
// because reproducing a live fake IPC listener from this package would
// require duplicating internal/api's OS-specific (unix-socket / named-pipe)
// test harness for one HTTP-status assertion.
func TestDaemonRespawnNoSupervisorReturns503(t *testing.T) {
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

	s := NewServer(Config{Port: 9125})
	rec := postDaemonEnvRaw(t, s, "/api/daemon/respawn", `{"task_name":"\\mcp-local-hub-memory-default"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no supervisor reachable; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SUPERVISOR_UNAVAILABLE") {
		t.Fatalf("body = %s, want SUPERVISOR_UNAVAILABLE error code", rec.Body.String())
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

// TestDaemonEnvGETResolvesManifestPortForTaskNameOnlyLegacyRow is bot PR #505 r5
// F1: a TASK-NAME-ONLY legacy row (blank Server/Daemon, no --server/--daemon argv,
// Port=0 — the pre-F5-heal shape) must show its MANIFEST port, not 0. The handler
// recovers time/default from the task name (ParseManagedTaskName) and threads it
// into the descriptor it hands the port owner; before the fix it passed the raw
// descriptor and the owner (which refuses task-name parsing) returned 0.
func TestDaemonEnvGETResolvesManifestPortForTaskNameOnlyLegacyRow(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			// No Server/Daemon fields and no Args — identity is recoverable ONLY
			// from the task name. time@9128 is a shipped embedded manifest.
			TaskName: `\mcp-local-hub-time-default`,
			Port:     0,
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
			Port     int    `json:"port"`
		} `json:"daemons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Daemons) != 1 {
		t.Fatalf("daemons len = %d, want 1: %+v", len(body.Daemons), body)
	}
	got := body.Daemons[0]
	if got.Server != "time" || got.Daemon != "default" {
		t.Fatalf("recovered identity = %s/%s, want time/default", got.Server, got.Daemon)
	}
	if got.Port != 9128 {
		t.Fatalf("port = %d, want 9128 (manifest port resolved via task-name-recovered identity, F1)", got.Port)
	}
}

// TestDaemonEnvGETPartialDaemonArgvStaysUnresolved is codex PR #505 r5 P3: a
// PARTIAL daemon-shaped argv (`daemon --server time` with NO --daemon) is a
// corrupt descriptor the port owner deliberately rejects. The task-name-only F1
// synthesis must NOT paper over it — a daemon-shaped argv is the owner's
// authority, so the port stays 0 (not a spurious manifest 9128). Only a TRUE
// task-name-only row (no daemon argv at all) gets the task-name synthesis.
func TestDaemonEnvGETPartialDaemonArgvStaysUnresolved(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: `\mcp-local-hub-time-default`,
			Port:     0,
			// Daemon-shaped argv but MISSING --daemon → corrupt/partial. The owner
			// rejects it; synthesis must not override the rejection.
			Args: []string{"daemon", "--server", "time"},
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
			Port int `json:"port"`
		} `json:"daemons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Daemons) != 1 {
		t.Fatalf("daemons len = %d, want 1: %+v", len(body.Daemons), body)
	}
	if body.Daemons[0].Port != 0 {
		t.Fatalf("port = %d, want 0 (partial daemon argv is owner-rejected, not synthesized)", body.Daemons[0].Port)
	}
}
