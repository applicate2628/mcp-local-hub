package api

import (
	"testing"
)

// TestEnrichStatusFillsPortFromManifest verifies enrichStatus maps the
// task-name suffix to the manifest's daemon.port field (no process poll).
func TestEnrichStatusFillsPortFromManifest(t *testing.T) {
	tmp := t.TempDir()
	makeFakeManifest(t, tmp+"/memory", "memory", 9123)
	makeFakeManifest(t, tmp+"/serena", "serena", 9121)

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-memory-default`},
		{TaskName: `\mcp-local-hub-serena-claude`},
		{TaskName: `\mcp-local-hub-godbolt-default`}, // no manifest in tmp — port stays 0
	}

	enrichStatus(rows, tmp)

	if rows[0].Port != 9123 {
		t.Errorf("memory.Port: got %d, want 9123", rows[0].Port)
	}
	if rows[1].Port != 9121 {
		t.Errorf("serena.Port: got %d, want 9121 (first daemon in manifest)", rows[1].Port)
	}
	if rows[2].Port != 0 {
		t.Errorf("godbolt.Port: got %d, want 0 (no manifest)", rows[2].Port)
	}
}
