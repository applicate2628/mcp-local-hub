package gdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	setupMI                 = "(gdb) \n^done\n(gdb) \n^done\n(gdb) \n"
	setupAndVersionCommands = "-gdb-set confirm off\n-gdb-set pagination off\n-gdb-version\n"
)

func TestStartTool_VersionFromInitializedMI(t *testing.T) {
	for _, tc := range []struct {
		name, versionReply, want string
	}{
		{
			name: "first decoded console line",
			versionReply: "@\"target output\\n\"\n&\"log output\\n\"\n*stopped,reason=\"breakpoint-hit\"\n" +
				"raw unrelated text\n~\"\\r\\n   \\r\\nGNU gdb (Vendor \"\n" +
				"~\"Build \\\"X\\\") 99.0\\r\\nCopyright ignored\\n\"\n70^done\n",
			want: "GNU gdb (Vendor Build \"X\") 99.0",
		},
		{name: "completed error", versionReply: "~\"do not save this line\\n\"\n71^error,msg=\"version unavailable\"\n"},
		{name: "empty console", versionReply: "@\"target output\\n\"\n~\" \\r\\n\"\n^done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs := newTestServer(t, "")
			var writes strings.Builder
			starts := 0
			gs.startSession = func(ctx context.Context, path, _ string) (*session, error) {
				starts++
				s := newFakeSession(t, setupMI+tc.versionReply+"(gdb) \n~\"still usable\\n\"\n^done\n")
				s.gdbPath = path
				s.stdin = nopWriteCloser{&writes}
				return s, s.completeStartup(ctx, nil)
			}
			res, err := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
			if err != nil || res.IsError {
				t.Fatalf("gdb_start: %+v, %v", res, err)
			}
			var payload map[string]string
			if err := json.Unmarshal([]byte(contentText(t, res)), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 3 || payload["session_id"] != "gdb-1" || payload["gdb_path"] != "C:\\fake\\gdb.exe" {
				t.Fatalf("unexpected start wire fields: %v", payload)
			}
			if version, ok := payload["version"]; !ok || version != tc.want {
				t.Errorf("version = %q, want %q", version, tc.want)
			}
			if starts != 1 || writes.String() != setupAndVersionCommands {
				t.Fatalf("startup acquired %d fake sessions and wrote %q", starts, writes.String())
			}
			res, err = gs.commandTool(context.Background(), newRequest(t, map[string]any{
				"session_id": payload["session_id"], "command": "info inferiors",
			}))
			if err != nil || res.IsError || contentText(t, res) != "still usable\n" {
				t.Fatalf("post-start command result: %+v, %v", res, err)
			}
		})
	}
}

func TestStartTool_VersionTerminalReplyFailsStartup(t *testing.T) {
	for _, tc := range []struct {
		name, versionReply, text string
		cause                    error
	}{
		{"missing result", "~\"partial version\\n\"\n", "closed its output stream before the result", io.EOF},
		{"unterminated error", "72^error,msg=\"incomplete\"", "closed its output stream before the result", io.EOF},
		{"debugger exit", "~\"discard this version\\n\"\n73^exit\n", "gdb exited during startup", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs := newTestServer(t, "")
			var writes strings.Builder
			s := newFakeSession(t, setupMI+tc.versionReply)
			s.stdin = nopWriteCloser{&writes}
			var startupErr error
			gs.startSession = func(ctx context.Context, _, _ string) (*session, error) {
				startupErr = s.completeStartup(ctx, nil)
				return s, startupErr
			}
			res, err := gs.startTool(context.Background(), newRequest(t, map[string]any{}))
			if err != nil || !res.IsError || !strings.Contains(contentText(t, res), tc.text) {
				t.Fatalf("expected startup failure containing %q: %+v, %v", tc.text, res, err)
			}
			if tc.cause != nil && !errors.Is(startupErr, tc.cause) {
				t.Errorf("startup cause %v missing from %v", tc.cause, startupErr)
			}
			waitForTestEvent(t, s.done)
			if !s.result.settled() || gs.lookup("gdb-1") != nil || s.version != "" || writes.String() != setupAndVersionCommands {
				t.Fatal("terminal version reply published a session or lost its original cleanup")
			}
		})
	}
}

func TestStartTool_VersionCancellationSettlesOriginalOwner(t *testing.T) {
	gs := newTestServer(t, "")
	readEntered, releaseRead := make(chan struct{}), make(chan struct{})
	waitRelease := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRead) })
	releaseWait := sync.OnceFunc(func() { close(waitRelease) })
	var writes strings.Builder
	cause := errors.New("cancel version query")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(cause)

	initial := strings.NewReader(setupMI + "~\"partial version\\n\"\n")
	reader := interruptedRead{testReadCloser{Reader: testReaderFunc(func(p []byte) (int, error) {
		if initial.Len() != 0 {
			return initial.Read(p)
		}
		close(readEntered)
		<-releaseRead
		return copy(p, "74^error"), os.ErrDeadlineExceeded
	})}, wakeOnExpiredDeadline(release)}
	s := newSession(nopWriteCloser{&writes}, reader, func() error {
		t.Error("startup forced the original owner in the gated fixture")
		return errors.New("unexpected force")
	})
	waits := 0
	go s.reap(func() (*os.ProcessState, error) {
		waits++
		<-waitRelease
		return new(os.ProcessState), nil
	})
	t.Cleanup(func() {
		release()
		releaseWait()
		waitForTestEvent(t, s.done)
	})
	var startupErr error
	gs.startSession = func(ctx context.Context, _, _ string) (*session, error) {
		startupErr = s.completeStartup(ctx, nil)
		return s, startupErr
	}

	req := newRequest(t, map[string]any{})
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		res, err := gs.startTool(ctx, req)
		result <- outcome{res, err}
	}()
	waitForTestEvent(t, readEntered)
	cancel(cause)
	waitForTestEvent(t, s.closing)
	select {
	case <-result:
		t.Fatal("startup completed before original wait was released")
	default:
	}
	releaseWait()
	select {
	case got := <-result:
		if got.err != nil || !got.result.IsError || !strings.Contains(contentText(t, got.result), cause.Error()) {
			t.Fatalf("canceled start wire result: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not settle after original wait returned")
	}
	if !errors.Is(startupErr, cause) || !errors.Is(startupErr, os.ErrDeadlineExceeded) {
		t.Fatalf("startup lost typed cancellation or I/O cause: %v", startupErr)
	}
	if !s.result.settled() || waits != 1 || gs.lookup("gdb-1") != nil || s.version != "" || writes.String() != setupAndVersionCommands {
		t.Fatal("canceled version query published a session or lost original owner settlement")
	}
}
