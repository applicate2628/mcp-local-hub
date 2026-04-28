package api

import (
	"os"
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

// TestParseTaskName locks in the task-name convention from install.go
// (and the scheduler_mgmt rewriter that consumes it). Tricky cases:
//   - weekly-refresh is a two-word daemon name, not two separate segments
//   - hyphenated server names work as long as the daemon is a single word
//     (default behaviour — LastIndex split is unambiguous when no daemon
//     contains '-')
func TestParseTaskName(t *testing.T) {
	cases := []struct {
		in         string
		wantSrv    string
		wantDaemon string
	}{
		{`\mcp-local-hub-serena-weekly-refresh`, "serena", "weekly-refresh"},
		{`\mcp-local-hub-memory-default`, "memory", "default"},
		{`\mcp-local-hub-serena-claude`, "serena", "claude"},
		{`\mcp-local-hub-paper-search-mcp-default`, "paper-search-mcp", "default"},
		{`\mcp-local-hub-paper-search-mcp-weekly-refresh`, "paper-search-mcp", "weekly-refresh"},
		{`mcp-local-hub-memory-default`, "memory", "default"}, // no leading backslash
		{`\mcp-local-hub-bareword`, "bareword", ""},
		{`\some-other-task`, "", ""}, // foreign prefix → empty
		// Hub-wide weekly-refresh task (from WeeklyRefreshSet) has no
		// per-server prefix. Must parse to (server="", daemon="weekly-refresh").
		// Without the exact-match short-circuit, the -weekly-refresh suffix
		// rule would attribute it to server="weekly-refresh", daemon=""
		// which is wrong — it's a hub-wide job, not a per-server daemon.
		{`\mcp-local-hub-weekly-refresh`, "", "weekly-refresh"},
		{`mcp-local-hub-weekly-refresh`, "", "weekly-refresh"},
	}
	for _, tc := range cases {
		gotSrv, gotDaemon := parseTaskName(tc.in)
		if gotSrv != tc.wantSrv || gotDaemon != tc.wantDaemon {
			t.Errorf("parseTaskName(%q) = (%q, %q), want (%q, %q)",
				tc.in, gotSrv, gotDaemon, tc.wantSrv, tc.wantDaemon)
		}
	}
}

// TestEnrichStatusWithRegistry_OrphanGlobalDaemonPreservesRawState
// guards DM-1: a non-lazy-proxy global daemon row whose manifest is
// not in this binary's embed (Port=0) must keep the raw scheduler
// "Running" state instead of being mis-labeled as "Starting" forever.
//
// Real production case: the user's installed mcphub.exe owns a Task
// Scheduler entry for `mcp-local-hub-foo-default`, but the dev binary
// (running `go run` from a checkout missing servers/foo/) issues the
// status query. The dev binary can't resolve foo's port, so Port=0;
// without DM-1, deriveState would render foo as permanently
// "Starting" while the actual installed daemon is healthy.
func TestEnrichStatusWithRegistry_OrphanGlobalDaemonPreservesRawState(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	// Manifest dir is empty — every global row's port stays 0.
	rows := []DaemonStatus{
		{
			TaskName: `\mcp-local-hub-unknown-server-default`,
			State:    "Running",
			NextRun:  "",
		},
	}
	enrichStatusWithRegistry(rows, dir, regPath)
	if rows[0].State != "Running" {
		t.Errorf("orphan global daemon (Port=0): State = %q, want %q (raw scheduler state must be preserved when manifest is unknown)",
			rows[0].State, "Running")
	}
	if rows[0].Port != 0 {
		t.Errorf("expected Port=0 for unknown manifest; got %d", rows[0].Port)
	}
}

// TestEnrichStatusWithRegistry_SelfPIDIsNotAlive guards DM-2: when
// netstat finds the running mcphub process bound to a daemon's port
// (port collision: e.g. wolfram manifest declares 9125 and mcphub
// GUI's --port also defaults to 9125), the row must NOT be reported
// as alive. Counting the GUI as the daemon would render a misleading
// "Running" with the GUI's PID/RAM/uptime, hiding the real failure
// from the operator.
func TestEnrichStatusWithRegistry_SelfPIDIsNotAlive(t *testing.T) {
	dir := t.TempDir()
	makeFakeManifest(t, dir+"/wolfram", "wolfram", 9125)

	origBatch := lookupProcessBatch
	defer func() { lookupProcessBatch = origBatch }()
	lookupProcessBatch = func(ports []int) map[int]struct {
		PID       int
		RAMBytes  uint64
		UptimeSec int64
	} {
		out := make(map[int]struct {
			PID       int
			RAMBytes  uint64
			UptimeSec int64
		})
		for _, p := range ports {
			out[p] = struct {
				PID       int
				RAMBytes  uint64
				UptimeSec int64
			}{PID: os.Getpid(), RAMBytes: 12345, UptimeSec: 67}
		}
		return out
	}

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-wolfram-default`, State: "Running"},
	}
	enrichStatus(rows, dir)

	// PID, RAM, Uptime must NOT be populated from the self-PID.
	if rows[0].PID != 0 {
		t.Errorf("self-PID leaked into row: PID = %d, want 0", rows[0].PID)
	}
	if rows[0].RAMBytes != 0 {
		t.Errorf("self-RAM leaked into row: RAMBytes = %d, want 0", rows[0].RAMBytes)
	}
	if rows[0].UptimeSec != 0 {
		t.Errorf("self-uptime leaked into row: UptimeSec = %d, want 0", rows[0].UptimeSec)
	}
	// alive=false → raw "Running" with no future trigger maps to "Starting"
	// via deriveState. The test asserts the row is NOT mis-marked Running.
	if rows[0].State == "Running" {
		t.Errorf("State = %q after self-PID skip; must not be %q (alive should be false)",
			rows[0].State, "Running")
	}
}

// TestEnrichStatusWithRegistry_ForeignPIDIsAlive ensures the DM-2 fix
// is a strict refinement of the previous behavior: a non-self PID at
// the daemon's port still produces alive=true and populates
// PID/RAM/Uptime exactly like before. Regression guard against an
// over-broad selfPID skip that would suppress every healthy daemon.
func TestEnrichStatusWithRegistry_ForeignPIDIsAlive(t *testing.T) {
	dir := t.TempDir()
	makeFakeManifest(t, dir+"/serena", "serena", 9121)

	const foreignPID = 999999 // pid that's not the test process
	if foreignPID == os.Getpid() {
		t.Skip("test PID happens to be 999999")
	}

	origBatch := lookupProcessBatch
	defer func() { lookupProcessBatch = origBatch }()
	lookupProcessBatch = func(ports []int) map[int]struct {
		PID       int
		RAMBytes  uint64
		UptimeSec int64
	} {
		out := make(map[int]struct {
			PID       int
			RAMBytes  uint64
			UptimeSec int64
		})
		for _, p := range ports {
			out[p] = struct {
				PID       int
				RAMBytes  uint64
				UptimeSec int64
			}{PID: foreignPID, RAMBytes: 50_000_000, UptimeSec: 3600}
		}
		return out
	}

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-serena-claude`, State: "Running"},
	}
	enrichStatus(rows, dir)

	if rows[0].PID != foreignPID {
		t.Errorf("foreign-PID daemon: PID = %d, want %d", rows[0].PID, foreignPID)
	}
	if rows[0].RAMBytes != 50_000_000 {
		t.Errorf("foreign-PID daemon: RAMBytes = %d, want 50000000", rows[0].RAMBytes)
	}
	if rows[0].State != "Running" {
		t.Errorf("foreign-PID daemon: State = %q, want Running", rows[0].State)
	}
}

// TestEnrichStatusWithRegistry_MaintenanceRowsBypassPortGuard guards the
// Codex r1 P2 finding on PR #21 (DM-1 narrowing): the hub-wide
// `mcp-local-hub-weekly-refresh` task is intentionally portless, and
// status consumers rely on deriveState to convert its raw scheduler
// state ("Ready" + future trigger) into the user-facing "Scheduled"
// label. The DM-1 guard skipped deriveState for every Port==0 row,
// which leaked raw "Ready" for the maintenance task and broke the
// dashboard's maintenance-job state badge.
//
// The fix narrows the guard to `Port==0 && !IsMaintenance` — this test
// asserts a maintenance row with raw "Ready" and a future NextRun
// renders as "Scheduled" (post-deriveState), proving the guard no
// longer over-skips. The companion DM-1 test
// (TestEnrichStatusWithRegistry_OrphanGlobalDaemonPreservesRawState)
// still passes because that row is non-maintenance so the guard fires
// for it as before.
func TestEnrichStatusWithRegistry_MaintenanceRowsBypassPortGuard(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"

	rows := []DaemonStatus{
		// Hub-wide weekly refresh — server="", daemon="weekly-refresh".
		// IsMaintenance is set in the first pass of enrichStatus.
		{
			TaskName: `\mcp-local-hub-weekly-refresh`,
			State:    "Ready",
			NextRun:  "04.05.2026 3:00:00",
		},
		// Per-server weekly refresh — same maintenance treatment.
		{
			TaskName: `\mcp-local-hub-serena-weekly-refresh`,
			State:    "Ready",
			NextRun:  "04.05.2026 3:00:00",
		},
	}
	enrichStatusWithRegistry(rows, dir, regPath)

	for i, row := range rows {
		if !row.IsMaintenance {
			t.Errorf("rows[%d] (%s): IsMaintenance = false, want true",
				i, row.TaskName)
		}
		if row.Port != 0 {
			t.Errorf("rows[%d] (%s): Port = %d, want 0 (maintenance task)",
				i, row.TaskName, row.Port)
		}
		if row.State != "Scheduled" {
			t.Errorf("rows[%d] (%s): State = %q, want %q (deriveState must run for maintenance rows; raw Ready + future trigger → Scheduled)",
				i, row.TaskName, row.State, "Scheduled")
		}
	}
}

// TestEnrichStatusWithRegistry_MaintenanceRowsNoTriggerBecomeStopped
// verifies the same bypass produces the correct "Stopped" state when
// the maintenance task has no future trigger (deriveState's other
// branch). Without the bypass this row would also leak raw "Ready"
// to the GUI.
func TestEnrichStatusWithRegistry_MaintenanceRowsNoTriggerBecomeStopped(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"

	rows := []DaemonStatus{
		{
			TaskName: `\mcp-local-hub-weekly-refresh`,
			State:    "Ready",
			NextRun:  "", // no upcoming trigger
		},
	}
	enrichStatusWithRegistry(rows, dir, regPath)

	if rows[0].State != "Stopped" {
		t.Errorf("maintenance row with no trigger: State = %q, want %q",
			rows[0].State, "Stopped")
	}
}

// TestDeriveState documents the four derived-state labels. lookupProcess
// is nil in unit tests so `alive` is always false at the enrichStatus
// boundary — exercise deriveState directly.
func TestDeriveState(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		alive   bool
		nextRun string
		wantOut string
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
