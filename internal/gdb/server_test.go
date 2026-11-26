package gdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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

func newTestServer(t *testing.T, miOutput string) *GdbServer {
	return &GdbServer{
		resolveGdbPath: func() string { return `C:\fake\gdb.exe` },
		startSession: func(_ context.Context, gdbPath, program string) (*session, error) {
			s := newFakeSession(t, miOutput)
			s.gdbPath = gdbPath
			return s, nil
		},
		probeVersion: func() (string, string, bool) {
			return `C:\fake\gdb.exe`, "GNU gdb (GDB) 14.2", true
		},
		sessions: map[string]*session{},
	}
}

// TestStartTool registers sessions under deterministic ids and reports their
// paths. The MI version handshake is exercised separately with initialized I/O.
func TestStartTool(t *testing.T) {
	gs := newTestServer(t, "^done\n(gdb) \n")
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
	// A second start increments the counter deterministically.
	res2, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	var got2 map[string]string
	_ = json.Unmarshal([]byte(contentText(t, res2)), &got2)
	if got2["session_id"] != "gdb-2" {
		t.Errorf("second session_id = %q, want gdb-2", got2["session_id"])
	}
}

// TestStartTool_HonorsGdbPathOverride: an explicit gdb_path is passed through to
// startSession instead of the resolver default.
func TestStartTool_HonorsGdbPathOverride(t *testing.T) {
	var seen string
	gs := newTestServer(t, "^done\n(gdb) \n")
	gs.startSession = func(_ context.Context, gdbPath, program string) (*session, error) {
		seen = gdbPath
		s := newFakeSession(t, "^done\n(gdb) \n")
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
	gs := newTestServer(t, "~\"hello\\n\"\n^done\n(gdb) \n")
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
	gs := newTestServer(t, "^done\n(gdb) \n")
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
	gs := newTestServer(t, "^error,msg=\"No symbol foo\"\n(gdb) \n")
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
	gs := newTestServer(t, "^done\n(gdb) \n")
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
	gs := newTestServer(t, "^done\n(gdb) \n")
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
	gs := newTestServer(t, "^done\n(gdb) \n")
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
	gs := newTestServer(t, "^done\n(gdb) \n")
	gs.startSession = func(_ context.Context, gdbPath, program string) (*session, error) {
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

func TestCommandTool_TerminalMISettlesSession(t *testing.T) {
	cases := []struct{ name, command, output string }{
		{"direct", "quit", "^exit\n"},
		{"token", "29-gdb-exit", "29^exit\n"},
		{"running", "-exec-run", "^running\n^exit\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := newTestServer(t, tc.output)
			gs.trackSession(newFakeSession(t, tc.output))
			req := newRequest(t, map[string]any{"session_id": "gdb-1", "command": tc.command})
			res, err := gs.commandTool(context.Background(), req)
			if err != nil || res.IsError {
				t.Fatalf("command failed: result=%+v err=%v", res, err)
			}
			if gs.lookup("gdb-1") != nil {
				t.Error("terminal MI result returned while session is still registered")
			}
		})
	}
}

type testReadCloser struct {
	io.Reader
	close func() error
}

func (r testReadCloser) Close() error {
	if r.close != nil {
		return r.close()
	}
	return nil
}

func (r testReadCloser) SetReadDeadline(time.Time) error { return nil }

type testReaderFunc func([]byte) (int, error)

func (r testReaderFunc) Read(p []byte) (int, error) { return r(p) }

func waitForTestEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test event")
	}
}

func TestCommandTool_WaitAndPipeSettlement(t *testing.T) {
	cases := []struct {
		name, output string
		cancel       bool
	}{
		{name: "terminal MI", output: "~\"ending\\n\"\n8^exit\n"},
		{name: "EOF after running", output: "^running\n~\"ending\\n\"\n"},
		{name: "cancel after MI exit", output: "^exit\n", cancel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := newTestServer(t, "")
			waitRelease, pipeRelease, pipeClosing := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseWait := sync.OnceFunc(func() { close(waitRelease) })
			releasePipe := sync.OnceFunc(func() { close(pipeRelease) })
			var writes, kills, waits atomic.Int32
			s := newSession(nopWriteCloser{testWriterFunc(func(p []byte) (int, error) {
				writes.Add(1)
				return len(p), nil
			})}, testReadCloser{strings.NewReader(tc.output), func() error {
				close(pipeClosing)
				<-pipeRelease
				return nil
			}}, func() error {
				kills.Add(1)
				return errors.New("unexpected force")
			})
			go s.reap(func() (*os.ProcessState, error) {
				waits.Add(1)
				<-waitRelease
				return new(os.ProcessState), nil
			})
			t.Cleanup(func() {
				releaseWait()
				releasePipe()
				waitForTestEvent(t, s.done)
			})
			id := gs.trackSession(s)
			req := newRequest(t, map[string]any{"session_id": id, "command": "-exec-next"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			commandDone := make(chan *mcp.CallToolResult, 1)
			go func() {
				res, _ := gs.commandTool(ctx, req)
				commandDone <- res
			}()
			readCommand := func() *mcp.CallToolResult {
				t.Helper()
				select {
				case res := <-commandDone:
					return res
				case <-time.After(5 * time.Second):
					t.Fatal("command did not complete")
					return nil
				}
			}
			checkPending := func() {
				t.Helper()
				if gs.lookup(id) != s {
					t.Fatal("owner was removed before settlement")
				}
				select {
				case res := <-commandDone:
					t.Fatalf("command returned before settlement: %+v", res)
				default:
				}
			}
			waitForTestEvent(t, s.closing)
			checkPending()
			afterClose, _ := gs.commandTool(context.Background(), req)
			if !afterClose.IsError || writes.Load() != 1 {
				t.Fatalf("closing session accepted another command: writes=%d result=%+v", writes.Load(), afterClose)
			}
			if tc.cancel {
				cancel()
				res := readCommand()
				if !res.IsError || !strings.Contains(contentText(t, res), "context canceled") || gs.lookup(id) != s {
					t.Fatalf("canceled observer lost pending owner: %+v", res)
				}
			}
			releaseWait()
			waitForTestEvent(t, pipeClosing)
			checkPending()
			releasePipe()
			waitForTestEvent(t, s.done)
			if !tc.cancel {
				res := readCommand()
				wantError := tc.name == "EOF after running"
				if res.IsError != wantError || !strings.Contains(contentText(t, res), "ending\n") {
					t.Fatalf("unexpected command result: %s", contentText(t, res))
				}
			}
			if gs.lookup(id) != nil || waits.Load() != 1 || kills.Load() != 0 {
				t.Fatalf("unexpected owner outcome: registered=%t waits=%d kills=%d",
					gs.lookup(id) != nil, waits.Load(), kills.Load())
			}
		})
	}
}

type testWriterFunc func([]byte) (int, error)

func (w testWriterFunc) Write(p []byte) (int, error) { return w(p) }

func TestProcessWait_PreservesActiveReaderAndExactRegistryEntry(t *testing.T) {
	gs := newTestServer(t, "")
	readStarted, readRelease := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	waitRelease := make(chan struct{})
	releaseWait := sync.OnceFunc(func() { close(waitRelease) })
	pipeClosed := make(chan struct{})
	output := strings.NewReader("~\"last record\\n\"\n^done\n")
	s := newSession(nopWriteCloser{io.Discard}, testReadCloser{testReaderFunc(func(p []byte) (int, error) {
		close(readStarted)
		<-readRelease
		return output.Read(p)
	}), func() error {
		close(pipeClosed)
		return nil
	}}, nil)
	go s.reap(func() (*os.ProcessState, error) {
		<-waitRelease
		return new(os.ProcessState), nil
	})
	t.Cleanup(func() {
		releaseRead()
		releaseWait()
		waitForTestEvent(t, s.done)
	})
	id := gs.trackSession(s)
	command := newRequest(t, map[string]any{"session_id": id, "command": "next"})
	commandDone := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := gs.commandTool(context.Background(), command)
		commandDone <- res
	}()
	waitForTestEvent(t, readStarted)
	releaseWait()
	waitForTestEvent(t, s.processDone)
	select {
	case <-pipeClosed:
		t.Fatal("process wait closed the output under an active reader")
	default:
	}

	replacement := newFakeSession(t, "^done\n")
	gs.mu.Lock()
	gs.sessions[id] = replacement
	gs.mu.Unlock()
	releaseRead()

	select {
	case res := <-commandDone:
		if res.IsError || contentText(t, res) != "last record\n" {
			t.Fatalf("buffered command result was lost: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not complete")
	}
	if gs.lookup(id) != replacement {
		t.Fatal("settlement removed a different entry")
	}
}

func TestCommandTool_InferiorExitKeepsSessionOpen(t *testing.T) {
	gs := newTestServer(t, "")
	id := gs.trackSession(newFakeSession(t,
		"^running\n=thread-group-exited,id=\"i1\",exit-code=\"0\"\n*stopped,reason=\"exited-normally\"\n"+
			"~\"ready\\n\"\n^done\n"))
	for _, tc := range []struct{ command, want string }{
		{"-exec-run", "[stopped: reason=exited-normally]\n"},
		{"info inferiors", "ready\n"},
	} {
		res, _ := gs.commandTool(context.Background(),
			newRequest(t, map[string]any{"session_id": id, "command": tc.command}))
		if res.IsError || contentText(t, res) != tc.want || gs.lookup(id) == nil {
			t.Fatalf("%s: unexpected result or closed session: %s", tc.command, contentText(t, res))
		}
	}
}

func TestTerminateTool_FailedForceRetainsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gs := newTestServer(t, "")
		waitRelease := make(chan struct{})
		releaseWait := sync.OnceFunc(func() { close(waitRelease) })
		var kills atomic.Int32
		s := newSession(nopWriteCloser{io.Discard}, testReadCloser{Reader: strings.NewReader("")}, func() error {
			kills.Add(1)
			return errors.New("fixture force failed")
		})
		go s.reap(func() (*os.ProcessState, error) {
			<-waitRelease
			return new(os.ProcessState), nil
		})
		t.Cleanup(func() {
			releaseWait()
			waitForTestEvent(t, s.done)
		})
		id := gs.trackSession(s)
		res, _ := gs.terminateTool(context.Background(), newRequest(t, map[string]any{"session_id": id}))
		if !res.IsError || !strings.Contains(contentText(t, res), "fixture force failed") || gs.lookup(id) != s {
			t.Fatalf("force failure was hidden or dropped ownership: %s", contentText(t, res))
		}
		if err := s.terminate(context.Background(), 0); err == nil || kills.Load() != 1 {
			t.Fatalf("re-entry did not preserve force failure: kills=%d err=%v", kills.Load(), err)
		}
		releaseWait()
		waitForTestEvent(t, s.done)
		if gs.lookup(id) != nil {
			t.Fatal("later original wait and pipe settlement did not remove session")
		}
	})
}

func TestRunShutdown_UnknownWaitRetainsFailedStart(t *testing.T) {
	gs := newTestServer(t, "")
	waitRelease := make(chan struct{})
	releaseWait := sync.OnceFunc(func() { close(waitRelease) })
	s := newSession(nopWriteCloser{io.Discard}, testReadCloser{Reader: strings.NewReader("")}, func() error {
		t.Error("shutdown attempted force after the sole Wait returned")
		return nil
	})
	go s.reap(func() (*os.ProcessState, error) {
		<-waitRelease
		return nil, errors.New("fixture wait failed")
	})
	t.Cleanup(func() {
		releaseWait()
		waitForTestEvent(t, s.done)
	})
	s.beginClosing()
	gs.startSession = func(context.Context, string, string) (*session, error) { return s, errFakeStart }
	res, _ := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
	if !res.IsError || !strings.Contains(contentText(t, res), "boom") || gs.lookup("gdb-1") != s {
		t.Fatalf("startup failure lost its owner or cause: %s", contentText(t, res))
	}
	releaseWait()
	waitForTestEvent(t, s.done)
	err := gs.shutdown()
	if err == nil || !strings.Contains(err.Error(), "fixture wait failed") || gs.lookup("gdb-1") != s {
		t.Fatalf("shutdown dropped unconfirmed process ownership: %v", err)
	}
}

func TestProcessWait_SettlesIdleSession(t *testing.T) {
	gs := newTestServer(t, "")
	waitRelease := make(chan struct{})
	releaseWait := sync.OnceFunc(func() { close(waitRelease) })
	state := new(os.ProcessState)
	s := newSession(nopWriteCloser{io.Discard}, testReadCloser{Reader: strings.NewReader("")}, nil)
	go s.reap(func() (*os.ProcessState, error) {
		<-waitRelease
		return state, nil
	})
	t.Cleanup(func() {
		releaseWait()
		waitForTestEvent(t, s.done)
	})
	id := gs.trackSession(s)
	releaseWait()
	waitForTestEvent(t, s.done)
	if gs.lookup(id) != nil || s.result.state != state {
		t.Fatal("idle exit did not retain the original wait result and remove the session")
	}
}

func TestTerminateTool_ConfirmedForceKeepsSuccessShape(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gs := newTestServer(t, "")
		waitRelease := make(chan struct{})
		releaseWait := sync.OnceFunc(func() { close(waitRelease) })
		waitErr := errors.New("fixture forced process exit status")
		var writes strings.Builder
		s := newSession(nopWriteCloser{&writes}, testReadCloser{Reader: strings.NewReader("")}, func() error {
			releaseWait()
			return nil
		})
		go s.reap(func() (*os.ProcessState, error) {
			<-waitRelease
			return new(os.ProcessState), waitErr
		})
		t.Cleanup(func() {
			releaseWait()
			waitForTestEvent(t, s.done)
		})
		id := gs.trackSession(s)
		res, _ := gs.terminateTool(context.Background(), newRequest(t, map[string]any{"session_id": id}))
		if res.IsError || contentText(t, res) != "{\"session_id\":\"gdb-1\",\"status\":\"terminated\"}" {
			t.Fatalf("termination returned %s", contentText(t, res))
		}
		if writes.String() != "-gdb-exit\n" || gs.lookup(id) != nil || s.result.waitErr != waitErr {
			t.Fatal("explicit force lost the graceful request, original status, or registry cleanup")
		}
	})
}
