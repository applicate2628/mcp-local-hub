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
