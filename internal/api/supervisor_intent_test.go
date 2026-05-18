package api

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSupervisorIntent_RoundTrip(t *testing.T) {
	// v0.5.0 Fix Group 5: WriteSupervisorIntent now flows through
	// the hardened secure-write pipeline (handle-bound DACL,
	// parent-dir gate, post-rename re-verify). Test temp dirs must
	// pass the parent-dir gate, which t.TempDir() alone may not on
	// machines whose %TEMP%/TMPDIR carries Authenticated Users (or
	// equivalent) write rights. hardenedTempDir installs the
	// allowlist-conforming DACL/mode the gate expects.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	want := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-16T18:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName:     `\mcp-local-hub-memory-default`,
				Server:       "memory",
				Daemon:       "default",
				Command:      "node",
				Args:         []string{"./mcp-memory-server.js"},
				Port:         9128,
				ManifestHash: "sha256:abc123",
			},
		},
		StrictMode: false,
	}
	if err := WriteSupervisorIntent(path, &want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Daemons[0].TaskName != `\mcp-local-hub-memory-default` {
		t.Fatalf("task_name not preserved: %q", got.Daemons[0].TaskName)
	}
	if got.Daemons[0].Port != 9128 {
		t.Fatalf("port not preserved: %d", got.Daemons[0].Port)
	}
}

func TestSupervisorIntent_FiltersLegacyWatchdogOneshot(t *testing.T) {
	// v0.4.x->v0.5.0 migration captured the `\mcp-local-hub-watchdog`
	// scheduled task into supervisor-intent.json as a daemon
	// descriptor. The watchdog's `--once` argv makes it exit
	// immediately, which combined with the supervisor's reconcile
	// respawn produces a wasteful watchdog spawn loop AND leaves a
	// duplicate "watchdog" row in GUI Dashboard alongside the legacy
	// Task Scheduler entry. ReadSupervisorIntent post-parses the
	// loaded file to strip such one-shot entries so existing broken
	// intent files self-heal on next read.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	on := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-18T00:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName: `\mcp-local-hub-memory-default`,
				Server:   "memory",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "--server", "memory"},
				Port:     9128,
			},
			{
				TaskName: `\mcp-local-hub-watchdog`,
				Command:  "mcphub",
				Args:     []string{"watchdog", "--once"},
			},
			{
				TaskName: `\mcp-local-hub-time-default`,
				Server:   "time",
				Daemon:   "default",
				Command:  "mcphub",
				Args:     []string{"daemon", "--server", "time"},
				Port:     9129,
			},
		},
	}
	if err := WriteSupervisorIntent(path, &on); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Daemons) != 2 {
		names := make([]string, 0, len(got.Daemons))
		for _, d := range got.Daemons {
			names = append(names, d.TaskName)
		}
		t.Fatalf("expected 2 daemons after watchdog filter, got %d: %v", len(got.Daemons), names)
	}
	for _, d := range got.Daemons {
		if d.TaskName == `\mcp-local-hub-watchdog` {
			t.Fatalf("watchdog entry leaked past filter: %+v", d)
		}
	}
	if got.Daemons[0].TaskName != `\mcp-local-hub-memory-default` ||
		got.Daemons[1].TaskName != `\mcp-local-hub-time-default` {
		t.Fatalf("daemon order not preserved across filter: %+v", got.Daemons)
	}
}

func TestSupervisorIntent_RejectsUnknownFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")
	body := `{"version":1,"updated_at":"2026-05-16T18:00:00.000000000Z","daemons":[],"strict_mode":false,"unknown_field":"x"}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	_, err := ReadSupervisorIntent(path)
	if err == nil {
		t.Fatalf("expected unknown-fields rejection, got nil")
	}
}
