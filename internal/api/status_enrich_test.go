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

// TestDeriveState documents the four derived-state labels. lookupProcess
// is nil in unit tests so `alive` is always false at the enrichStatus
// boundary — exercise deriveState directly.
func TestDeriveState(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		alive    bool
		nextRun  string
		wantOut  string
	}{
		{"alive overrides any raw state", "Ready", true, "N/A", "Running"},
		{"alive even when raw=Running (task still firing)", "Running", true, "", "Running"},
		{"raw=Running and dead → Starting (schtasks action mid-launch)", "Running", false, "N/A", "Starting"},
		{"raw=Ready, future trigger, dead → Scheduled (weekly task idle)", "Ready", false, "19.04.2026 3:00:00", "Scheduled"},
		{"raw=Ready, N/A trigger, dead → Stopped (logon daemon crashed)", "Ready", false, "N/A", "Stopped"},
		{"raw=Ready, empty trigger, dead → Stopped (no trigger)", "Ready", false, "", "Stopped"},
		{"raw=Ready, na lowercase, dead → Stopped (case-insensitive N/A)", "Ready", false, "n/a", "Stopped"},
		{"raw=Disabled passes through (no trigger will fire)", "Disabled", false, "", "Disabled"},
		{"raw=Queued passes through", "Queued", false, "", "Queued"},
	}
	for _, tc := range cases {
		got := deriveState(tc.raw, tc.alive, tc.nextRun)
		if got != tc.wantOut {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.wantOut)
		}
	}
}
