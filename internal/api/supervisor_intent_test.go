package api

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
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

// TestSupervisorIntent_FilterStrictWatchdogOnceOnly verifies the
// tightened filter predicate at supervisor_intent.go:isLegacyOneshotDaemon —
// only `["watchdog", "--once"]` (the exact migration artifact) is
// stripped. Defensive: a future long-lived `mcphub watchdog serve`
// daemon variant must NOT be filtered away by a too-broad match. PR
// #212 r2 code-review finding 5.
func TestSupervisorIntent_FilterStrictWatchdogOnceOnly(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	on := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-18T00:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-watchdog`, Command: "mcphub", Args: []string{"watchdog", "--once"}},      // FILTERED
			{TaskName: `\mcp-local-hub-watchdog-bare`, Command: "mcphub", Args: []string{"watchdog"}},           // KEPT — not --once
			{TaskName: `\mcp-local-hub-watchdog-serve`, Command: "mcphub", Args: []string{"watchdog", "serve"}}, // KEPT — future variant
			{TaskName: `\mcp-local-hub-watchdog-empty`, Command: "mcphub", Args: []string{}},                    // KEPT — no args
		},
	}
	if err := WriteSupervisorIntent(path, &on); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Daemons) != 3 {
		names := make([]string, 0, len(got.Daemons))
		for _, d := range got.Daemons {
			names = append(names, d.TaskName)
		}
		t.Fatalf("expected 3 daemons (only `watchdog --once` filtered), got %d: %v", len(got.Daemons), names)
	}
	for _, d := range got.Daemons {
		if d.TaskName == `\mcp-local-hub-watchdog` {
			t.Errorf("watchdog --once entry should be filtered, got %+v", d)
		}
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

// TestSupervisorIntent_RoundTrip_NilRuntimeSpecLegacyFile is the additive-
// field regression guard (design claim #2): an existing pre-RuntimeSpec
// supervisor-intent.json must decode through the NEW-binary
// ReadSupervisorIntent (which calls DisallowUnknownFields) with a nil
// RuntimeSpec and NO error, and re-marshal must OMIT the runtime_spec key
// (omitempty) so the on-disk shape is byte-identical for legacy daemons.
func TestSupervisorIntent_RoundTrip_NilRuntimeSpecLegacyFile(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")
	// A legacy intent body written before RuntimeSpec existed: no
	// runtime_spec key anywhere. DisallowUnknownFields must still accept it.
	body := `{"version":1,"updated_at":"2026-05-16T18:00:00.000000000Z","daemons":[` +
		`{"task_name":"\\mcp-local-hub-memory-default","server":"memory","daemon":"default","command":"node","args":["./mcp-memory-server.js"],"port":9128,"manifest_hash":"sha256:abc123"}` +
		`],"strict_mode":false}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("legacy file must decode through DisallowUnknownFields with nil RuntimeSpec; got error: %v", err)
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("got %d daemons; want 1", len(got.Daemons))
	}
	if got.Daemons[0].RuntimeSpec != nil {
		t.Errorf("legacy daemon must have nil RuntimeSpec; got %#v", got.Daemons[0].RuntimeSpec)
	}
	// Re-marshal: the absent RuntimeSpec must NOT serialize a runtime_spec
	// key (omitempty), preserving byte-shape for legacy rows.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "runtime_spec") {
		t.Errorf("nil RuntimeSpec must be omitted from JSON (omitempty); got: %s", string(raw))
	}
}

// TestSupervisorIntent_RoundTrip_WithRuntimeSpec asserts a daemon carrying a
// fully-populated RuntimeSpec marshals, decodes through the
// DisallowUnknownFields reader, and equals the original — every RuntimeSpec
// field is preserved (design §3 schema).
func TestSupervisorIntent_RoundTrip_WithRuntimeSpec(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-intent.json")

	want := SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-29T12:00:00.000000000Z",
		Daemons: []SupervisorDaemon{
			{
				TaskName:     `\mcp-local-hub-serena-deadbeef`,
				Server:       "serena",
				Daemon:       "deadbeef",
				Command:      `C:\bin\mcphub.exe`,
				Args:         []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\alpha`, "--port", "9121", "--task-name", `\mcp-local-hub-serena-deadbeef`},
				Port:         9121,
				Workspace:    `C:\work\alpha`,
				ManifestHash: "sha256:abc",
				RuntimeSpec: &DaemonRuntimeSpec{
					SpecVersion:   DaemonRuntimeSpecVersion,
					ChildCommand:  "uvx",
					ChildArgs:     []string{"--from", "git+https://example/serena", "serena", "start-mcp-server", "--project", `C:\work\alpha`, "--context", "codex"},
					EnvRefs:       map[string]string{"PYTHONUNBUFFERED": "1", "SERENA_TOKEN": "secret:SERENA_TOKEN"},
					UpstreamPort:  19121,
					ExternalPort:  9121,
					WorkspacePath: `C:\work\alpha`,
				},
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
	if len(got.Daemons) != 1 {
		t.Fatalf("got %d daemons; want 1", len(got.Daemons))
	}
	if !reflect.DeepEqual(got.Daemons[0].RuntimeSpec, want.Daemons[0].RuntimeSpec) {
		t.Errorf("RuntimeSpec round-trip mismatch:\n got=%#v\nwant=%#v", got.Daemons[0].RuntimeSpec, want.Daemons[0].RuntimeSpec)
	}
}
