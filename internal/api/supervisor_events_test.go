// Package api — tests for supervisor-events.log JSONL helper (v0.5.0
// Task 2.3). Mirrors the discipline of gui_event_log_test.go and
// watchdog_log_test.go but exercises the supervisor envelope shape:
// `event` discriminator + `task_name` identity field.
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSupervisorEvent_EnvelopeShape verifies the wire shape of one
// emitted event: schema_version "1", event discriminator, task_name
// identity field, body object. Mirrors gui_event_log.go:19-25 with
// the supervisor-specific additions (event + task_name).
func TestSupervisorEvent_EnvelopeShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "ipc-command",
		TaskName: `\mcp-local-hub-memory-default`,
		Body:     map[string]any{"cmd": "exit", "result": "ok"},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(string(raw), "\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["schema_version"] != "1" {
		t.Fatalf("schema_version: %v", got["schema_version"])
	}
	if got["event"] != "ipc-command" {
		t.Fatalf("event: %v", got["event"])
	}
	if got["source"] != "ipc" {
		t.Fatalf("source: %v", got["source"])
	}
	if got["severity"] != "info" {
		t.Fatalf("severity: %v", got["severity"])
	}
	if got["task_name"] != `\mcp-local-hub-memory-default` {
		t.Fatalf("task_name: %v", got["task_name"])
	}
	if _, ok := got["body"].(map[string]any); !ok {
		t.Fatalf("body not object: %T", got["body"])
	}
	if got["ts"] == "" || got["ts"] == nil {
		t.Fatalf("ts not auto-populated: %v", got["ts"])
	}
}

// TestSupervisorEvent_OversizeTruncation verifies the 16KB cap is
// enforced and that identity fields (event, source, task_name) are
// never truncated. Body fields take the hit per the watchdog_log.go
// precedent. The _truncated marker must be present.
func TestSupervisorEvent_OversizeTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	big := strings.Repeat("x", 32*1024) // 32KB single field, exceeds 16KB cap
	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "ipc-command",
		TaskName: `\mcp-local-hub-memory-default`,
		Body:     map[string]any{"large": big},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 17*1024 { // entry+newline ≤ 16KB+1024 buffer
		t.Fatalf("entry not truncated: %d bytes", len(raw))
	}
	s := string(raw)
	if !strings.Contains(s, `"_truncated":true`) {
		t.Fatalf("missing _truncated marker; got=%q", s)
	}
	// Identity fields MUST survive untouched per §35 of the precedent.
	if !strings.Contains(s, `"task_name":"\\mcp-local-hub-memory-default"`) {
		t.Fatalf("identity field task_name truncated; got=%q", s)
	}
	if !strings.Contains(s, `"event":"ipc-command"`) {
		t.Fatalf("identity field event truncated; got=%q", s)
	}
	if !strings.Contains(s, `"source":"ipc"`) {
		t.Fatalf("identity field source truncated; got=%q", s)
	}
}

// TestSupervisorEvent_Rotation verifies 10MB rotation: an oversize
// active log is renamed to .1 on next emit. Mirrors the precedent at
// gui_event_log.go:166-170 + watchdog_log.go:237-241.
func TestSupervisorEvent_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	// Pre-seed the active log over the rotation threshold (10MB).
	padding := make([]byte, supervisorEventLogRotateSize+1)
	for i := range padding {
		padding[i] = 'a'
	}
	if err := os.WriteFile(path, padding, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "supervisor-start",
		Body:     map[string]any{"version": "0.5.0"},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// .1 must exist (rotated padding) and active log must be small
	// (just the one new entry).
	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf(".1 not created: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active stat: %v", err)
	}
	if st.Size() >= supervisorEventLogRotateSize {
		t.Fatalf("active log not rotated: size=%d", st.Size())
	}
}

// TestSupervisorEvent_SchemaVersionAutoFilled verifies that callers
// who pass zero-value SchemaVersion get "1" injected.
func TestSupervisorEvent_SchemaVersionAutoFilled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Emit(SupervisorEvent{
		// Intentionally leave SchemaVersion / TS / Severity blank
		Source: "reconcile",
		Event:  "reconcile-tick",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(string(raw), "\n")), &got); err != nil {
		t.Fatal(err)
	}
	if got["schema_version"] != "1" {
		t.Fatalf("schema_version not auto-filled: %v", got["schema_version"])
	}
	if got["ts"] == "" || got["ts"] == nil {
		t.Fatalf("ts not auto-filled: %v", got["ts"])
	}
}
