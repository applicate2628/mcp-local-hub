package gdb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newRequest wraps raw JSON args as a CallToolRequest so tests can invoke the
// handlers without constructing the full MCP request plumbing. Mirrors the
// drmemory direct-construction style.
func newRequest(t *testing.T, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}}
}

func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty Content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] is not TextContent: %T", res.Content[0])
	}
	return tc.Text
}

// newTestServer builds a GdbServer whose startSession seam returns a fake
// session backed by the canned MI output, so no real gdb is spawned. The
// resolveGdbPath + probeVersion seams are also faked.
//
// The fake startSession populates s.version through the SAME startVersionProbe
// seam the production startSession uses (defaulted to the canned version in
// each test that asserts on it). It deliberately does NOT hardcode a version
// literal: an earlier revision did, which masked the production bug where
// gdb_start always returned version="" because the `-q`-suppressed banner was
// scraped for the version instead of a dedicated `--version` probe.
func newTestServer(miOutput string) *GdbServer {
	return &GdbServer{
		resolveGdbPath: func() string { return `C:\fake\gdb.exe` },
		startSession: func(gdbPath, program string) (*session, error) {
			s := newFakeSession(miOutput)
			s.gdbPath = gdbPath
			s.version = startVersionProbe(gdbPath)
			return s, nil
		},
		probeVersion: func() (string, string, bool) {
			return `C:\fake\gdb.exe`, "GNU gdb (GDB) 14.2", true
		},
		sessions: map[string]*session{},
	}
}

// withStartVersionProbe swaps the package-level startVersionProbe seam for the
// duration of a test (restored via t.Cleanup) so a test exercises the
// version-population path without spawning real gdb.
func withStartVersionProbe(t *testing.T, fn func(gdbPath string) string) {
	t.Helper()
	prev := startVersionProbe
	startVersionProbe = fn
	t.Cleanup(func() { startVersionProbe = prev })
}

// TestStartTool registers a session and returns the deterministic id + path +
// version. The version must be the non-empty string produced by the
// startVersionProbe seam — the production bug was that gdb_start always returned
// version="" because the `-q`-suppressed banner was scraped instead.
func TestStartTool(t *testing.T) {
	var probedPath string
	withStartVersionProbe(t, func(gdbPath string) string {
		probedPath = gdbPath
		return "GNU gdb (GDB) 14.2"
	})

	gs := newTestServer("^done\n(gdb) \n")
	res, err := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if res.IsError {
		t.Fatalf("startTool returned IsError: %s", contentText(t, res))
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal start result: %v", err)
	}
	if got["session_id"] != "gdb-1" {
		t.Errorf("session_id = %q, want gdb-1", got["session_id"])
	}
	if got["gdb_path"] != `C:\fake\gdb.exe` {
		t.Errorf("gdb_path = %q", got["gdb_path"])
	}
	if got["version"] != "GNU gdb (GDB) 14.2" {
		t.Errorf("version = %q, want the probe result (non-empty)", got["version"])
	}
	if probedPath != `C:\fake\gdb.exe` {
		t.Errorf("startVersionProbe got gdb_path %q, want the resolved path", probedPath)
	}

	// A second start increments the counter deterministically.
	res2, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	var got2 map[string]string
	_ = json.Unmarshal([]byte(contentText(t, res2)), &got2)
	if got2["session_id"] != "gdb-2" {
		t.Errorf("second session_id = %q, want gdb-2", got2["session_id"])
	}
}

// TestStartVersionProbeWired asserts the version a session carries is exactly
// what startVersionProbe returns for the resolved gdb path — the regression
// guard for the gdb_start version="" bug. It drives the full startTool path with
// an injected probe (no real gdb), so a future regression that drops the probe
// and re-scrapes the empty `-q` banner fails here.
func TestStartVersionProbeWired(t *testing.T) {
	withStartVersionProbe(t, func(gdbPath string) string {
		return "GNU gdb (GDB) 99.9 [probe]"
	})

	gs := newTestServer("^done\n(gdb) \n")
	res, err := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	var got map[string]string
	_ = json.Unmarshal([]byte(contentText(t, res)), &got)
	if got["version"] != "GNU gdb (GDB) 99.9 [probe]" {
		t.Errorf("version = %q, want the injected probe result", got["version"])
	}
	if got["version"] == "" {
		t.Error("version is empty — the gdb_start version='' regression is back")
	}
}

// TestStartTool_HonorsGdbPathOverride: an explicit gdb_path is passed through to
// startSession instead of the resolver default.
func TestStartTool_HonorsGdbPathOverride(t *testing.T) {
	var seen string
	gs := newTestServer("^done\n(gdb) \n")
	gs.startSession = func(gdbPath, program string) (*session, error) {
		seen = gdbPath
		s := newFakeSession("^done\n(gdb) \n")
		s.gdbPath = gdbPath
		return s, nil
	}

	_, err := gs.startTool(context.Background(), newRequest(t, map[string]any{"gdb_path": `D:\custom\gdb.exe`}))
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if seen != `D:\custom\gdb.exe` {
		t.Errorf("startSession got gdb_path %q, want the override", seen)
	}
}

// TestCommandTool drives a command through a registered session and returns the
// console output.
func TestCommandTool(t *testing.T) {
	gs := newTestServer("~\"hello\\n\"\n^done\n(gdb) \n")
	startRes, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	var start map[string]string
	_ = json.Unmarshal([]byte(contentText(t, startRes)), &start)

	res, err := gs.commandTool(context.Background(), newRequest(t, map[string]any{
		"session_id": start["session_id"],
		"command":    "print x",
	}))
	if err != nil {
		t.Fatalf("commandTool error: %v", err)
	}
	if res.IsError {
		t.Fatalf("commandTool returned IsError: %s", contentText(t, res))
	}
	if contentText(t, res) != "hello\n" {
		t.Errorf("command output = %q, want %q", contentText(t, res), "hello\n")
	}
}

// TestCommandTool_UnknownSession returns a tool error for an unknown id.
func TestCommandTool_UnknownSession(t *testing.T) {
	gs := newTestServer("^done\n(gdb) \n")
	res, err := gs.commandTool(context.Background(), newRequest(t, map[string]any{
		"session_id": "gdb-999",
		"command":    "print x",
	}))
	if err != nil {
		t.Fatalf("commandTool error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError for unknown session, got %s", contentText(t, res))
	}
	if !strings.Contains(contentText(t, res), "unknown session_id") {
		t.Errorf("error text = %q", contentText(t, res))
	}
}

// TestCommandTool_GdbError surfaces a gdb ^error result as a tool error result.
func TestCommandTool_GdbError(t *testing.T) {
	gs := newTestServer("^error,msg=\"No symbol foo\"\n(gdb) \n")
	startRes, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	var start map[string]string
	_ = json.Unmarshal([]byte(contentText(t, startRes)), &start)

	res, _ := gs.commandTool(context.Background(), newRequest(t, map[string]any{
		"session_id": start["session_id"],
		"command":    "print foo",
	}))
	if !res.IsError {
		t.Fatalf("expected IsError for gdb error, got %s", contentText(t, res))
	}
	if !strings.Contains(contentText(t, res), "No symbol foo") {
		t.Errorf("error text = %q", contentText(t, res))
	}
}

// TestListAndTerminate: a started session shows in the list, and terminate
// removes it.
func TestListAndTerminate(t *testing.T) {
	gs := newTestServer("^done\n(gdb) \n")
	startRes, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	var start map[string]string
	_ = json.Unmarshal([]byte(contentText(t, startRes)), &start)
	id := start["session_id"]

	listRes, _ := gs.listTool(context.Background(), newRequest(t, map[string]any{}))
	var list map[string][]string
	_ = json.Unmarshal([]byte(contentText(t, listRes)), &list)
	if len(list["sessions"]) != 1 || list["sessions"][0] != id {
		t.Errorf("list = %v, want [%s]", list["sessions"], id)
	}

	termRes, err := gs.terminateTool(context.Background(), newRequest(t, map[string]any{"session_id": id}))
	if err != nil {
		t.Fatalf("terminateTool error: %v", err)
	}
	if termRes.IsError {
		t.Fatalf("terminate returned IsError: %s", contentText(t, termRes))
	}

	listRes2, _ := gs.listTool(context.Background(), newRequest(t, map[string]any{}))
	var list2 map[string][]string
	_ = json.Unmarshal([]byte(contentText(t, listRes2)), &list2)
	if len(list2["sessions"]) != 0 {
		t.Errorf("after terminate, list = %v, want empty", list2["sessions"])
	}

	// Terminating again is an unknown-session tool error.
	termRes2, _ := gs.terminateTool(context.Background(), newRequest(t, map[string]any{"session_id": id}))
	if !termRes2.IsError {
		t.Errorf("double-terminate should be an unknown-session error")
	}
}

// TestStatusTool_Available reports availability from the injected probe.
func TestStatusTool_Available(t *testing.T) {
	gs := newTestServer("^done\n(gdb) \n")
	res, err := gs.statusTool(context.Background(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got["available"] != true {
		t.Errorf("available = %v, want true", got["available"])
	}
	if got["gdb_path"] != `C:\fake\gdb.exe` {
		t.Errorf("gdb_path = %v", got["gdb_path"])
	}
	if got["version"] != "GNU gdb (GDB) 14.2" {
		t.Errorf("version = %v", got["version"])
	}
}

// TestStatusTool_Unavailable reports the not-available shape when the probe
// fails (the exact failure mode GDB-MCP's python subprocess hit).
func TestStatusTool_Unavailable(t *testing.T) {
	gs := newTestServer("^done\n(gdb) \n")
	gs.probeVersion = func() (string, string, bool) {
		return `C:\fake\gdb.exe`, "", false
	}
	res, _ := gs.statusTool(context.Background(), newRequest(t, map[string]any{}))
	var got map[string]any
	_ = json.Unmarshal([]byte(contentText(t, res)), &got)
	if got["available"] != false {
		t.Errorf("available = %v, want false", got["available"])
	}
	if got["version"] != "" {
		t.Errorf("version = %v, want empty on unavailable", got["version"])
	}
}

// TestStartTool_StartFailureSurfaced: a startSession error becomes a tool error
// result, not a panic or empty success.
func TestStartTool_StartFailureSurfaced(t *testing.T) {
	gs := newTestServer("^done\n(gdb) \n")
	gs.startSession = func(gdbPath, program string) (*session, error) {
		return nil, errFakeStart
	}
	res, err := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on start failure, got %s", contentText(t, res))
	}
	if !strings.Contains(contentText(t, res), "boom") {
		t.Errorf("error text = %q, want the start failure reason", contentText(t, res))
	}
}

// errFakeStart is a sentinel start error for TestStartTool_StartFailureSurfaced.
var errFakeStart = fakeErr("boom")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
