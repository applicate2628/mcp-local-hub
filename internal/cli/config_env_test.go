package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
)

func TestConfigEnvSetWritesOperatorOverlayForSingleDaemonServer(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Port:     9123,
	})

	var out bytes.Buffer
	if err := runConfigEnvSet(stateDir, "memory", "MEMORY_FILE_PATH", `D:\memory\memory.jsonl`, &out); err != nil {
		t.Fatalf("runConfigEnvSet: %v", err)
	}

	ov, err := daemon_env_overlay.Load(filepath.Join(stateDir, overlayBaseName))
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	row, ok := ov.Daemons[`\mcp-local-hub-memory-default`]
	if !ok {
		t.Fatalf("memory overlay row missing: %+v", ov.Daemons)
	}
	if got := row.Env["MEMORY_FILE_PATH"]; got != `D:\memory\memory.jsonl` {
		t.Fatalf("MEMORY_FILE_PATH = %q, want D:\\memory\\memory.jsonl", got)
	}
	if row.Source != "operator" {
		t.Fatalf("row source = %q, want operator", row.Source)
	}
	if !strings.Contains(strings.ToLower(out.String()), "restart") {
		t.Fatalf("operator output must mention restart requirement, got %q", out.String())
	}
}

func TestConfigEnvSetRejectsAmbiguousMultiDaemonServer(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	seedConfigEnvIntent(t, stateDir,
		api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-alpha`, Server: "serena", Daemon: "alpha"},
		api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-beta`, Server: "serena", Daemon: "beta"},
	)

	var out bytes.Buffer
	err := runConfigEnvSet(stateDir, "serena", "PYTHONUNBUFFERED", "1", &out)
	if err == nil {
		t.Fatal("runConfigEnvSet returned nil for ambiguous server; want error")
	}
	if !strings.Contains(err.Error(), "server/daemon") {
		t.Fatalf("ambiguous error = %v, want server/daemon hint", err)
	}
}

func TestConfigEnvUnsetRemovesOnlyRequestedKey(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	seedConfigEnvIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
	})
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	if err := daemon_env_overlay.WriteOverlay(overlayPath, func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env: map[string]string{
				"MEMORY_FILE_PATH": `D:\memory\memory.jsonl`,
				"EXTRA":            "kept",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	var out bytes.Buffer
	if err := runConfigEnvUnset(stateDir, "memory", "MEMORY_FILE_PATH", &out); err != nil {
		t.Fatalf("runConfigEnvUnset: %v", err)
	}
	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	row := ov.Daemons[`\mcp-local-hub-memory-default`]
	if _, ok := row.Env["MEMORY_FILE_PATH"]; ok {
		t.Fatalf("MEMORY_FILE_PATH still present after unset: %+v", row.Env)
	}
	if got := row.Env["EXTRA"]; got != "kept" {
		t.Fatalf("EXTRA = %q, want kept", got)
	}
}

func TestDaemonEnvRefsWithOverlayWinsBeforeSecretResolve(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
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

	got, err := daemonEnvRefsWithOverlay("memory", "default", map[string]string{
		"MEMORY_FILE_PATH": "secret:OLD_MEMORY_PATH",
		"API_TOKEN":        "secret:UNCHANGED",
	})
	if err != nil {
		t.Fatalf("daemonEnvRefsWithOverlay: %v", err)
	}
	if got["MEMORY_FILE_PATH"] != `D:\memory\memory.jsonl` {
		t.Fatalf("MEMORY_FILE_PATH = %q, want override", got["MEMORY_FILE_PATH"])
	}
	if got["API_TOKEN"] != "secret:UNCHANGED" {
		t.Fatalf("API_TOKEN = %q, want untouched secret ref", got["API_TOKEN"])
	}
}

func seedConfigEnvIntent(t *testing.T, stateDir string, daemons ...api.SupervisorDaemon) {
	t.Helper()
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: daemons}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
}
