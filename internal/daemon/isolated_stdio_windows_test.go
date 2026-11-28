//go:build windows

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

const isolatedFixtureMode = "MCPHUB_TEST_PRIVATE_STDIO_FIXTURE"
const isolatedFixtureGate = "MCPHUB_TEST_PRIVATE_STDIO_GATE"
const isolatedFixtureStderrBytes = 2 << 20

func TestOwnedIsolatedStdioFixture(t *testing.T) {
	mode := os.Getenv(isolatedFixtureMode)
	if mode == "" {
		return
	}
	if mode == "grace-member" {
		runStdioGraceMember()
	}
	if mode == "grace-wrapper" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(36)
		}
		member := exec.Command(exe, "-test.run=^TestOwnedIsolatedStdioFixture$")
		process.NoConsole(member)
		member.Env = composeChildEnv(map[string]string{isolatedFixtureMode: "grace-member"}, nil)
		if err := member.Start(); err != nil {
			os.Exit(37)
		}
		// The original at-create Job owns the member after the wrapper
		// exits. No member inherits the wrapper's provider stdio pipes.
		if err := member.Process.Release(); err != nil {
			os.Exit(38)
		}
	}
	noisy := bytes.Repeat([]byte{'s', 'y', 'n', 't', 'h', 0xff}, isolatedFixtureStderrBytes/6+1)[:isolatedFixtureStderrBytes]
	if n, err := os.Stderr.Write(noisy); err != nil || n != len(noisy) {
		os.Exit(31)
	}
	scan := bufio.NewScanner(os.Stdin)
	state := 0
	var waiting json.RawMessage
	for scan.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			os.Exit(32)
		}
		var reply string
		switch msg.Method {
		case "initialize":
			reply = `"result":{"protocolVersion":"2025-06-18","capabilities":{}}`
		case "notifications/cancelled":
			if len(waiting) != 0 && bytes.Equal(waiting, msg.Params.ID) {
				msg.ID = waiting
				waiting = nil
				reply = fmt.Sprintf(`"result":{"canceled":%s}`, msg.Params.ID)
			}
		case "tools/list":
			reply = `"result":{"tools":[]}`
		case "tools/call":
			state++
			reply = fmt.Sprintf(`"result":{"structuredContent":{"count":%d}}`, state)
		case "fixture/error":
			reply = `"error":{"code":-32000,"message":"synth-private-error"}`
		case "fixture/wait-for-cancel":
			waiting = msg.ID
		}
		if reply != "" {
			if _, err := fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,%s}\n", msg.ID, reply); err != nil {
				os.Exit(33)
			}
		}
	}
	if scan.Err() != nil {
		os.Exit(34)
	}
	if mode == "ignore-eof" {
		<-time.After(time.Minute)
		os.Exit(35)
	}
	os.Exit(0)
}

func runStdioGraceMember() {
	root := os.Getenv(isolatedFixtureGate)
	if root == "" || os.WriteFile(filepath.Join(root, "entered"), nil, 0600) != nil {
		os.Exit(39)
	}
	check := time.NewTicker(5 * time.Millisecond)
	defer check.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			os.Exit(40)
		case <-check.C:
			_, err := os.Stat(filepath.Join(root, "release"))
			if err == nil {
				if os.WriteFile(filepath.Join(root, "exited-cooperatively"), nil, 0600) != nil {
					os.Exit(41)
				}
				os.Exit(0)
			}
			if !os.IsNotExist(err) {
				os.Exit(42)
			}
		}
	}
}

type safeFixtureDiagnostic struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (d *safeFixtureDiagnostic) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.Write(data)
}

func (d *safeFixtureDiagnostic) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.String()
}

func nativeIsolatedStdio(t *testing.T, mode string, slots int) (*IsolatedStdioSessions, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	launch, err := CaptureStdioLaunchContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "forbidden-provider-stderr.log")
	p, err := launch.NewIsolatedStdioSessions(HostConfig{
		Command: exe, Args: []string{"-test.run=^TestOwnedIsolatedStdioFixture$"},
		Env: map[string]string{isolatedFixtureMode: mode}, LogPath: logPath,
	}, IsolatedStdioLimits{
		TreeCapacity: 2 * slots, TreeCost: 2, RetryAfterSeconds: 2,
		MaxRequestIDs: 16, MaxIDBytes: 32, Startup: 4 * time.Second,
		Operation: 2 * time.Second, Idle: 10 * time.Second,
		Teardown: 4 * time.Second, EOFGrace: 500 * time.Millisecond,
	}, func(r *http.Request) IsolatedStdioRole {
		switch r.Header.Get("X-Owned-Fixture-Grant") {
		case "hub":
			return IsolatedStdioHub
		case "discovery":
			return IsolatedStdioDiscovery
		default:
			return IsolatedStdioDenied
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, release := context.WithTimeout(context.Background(), 5*time.Second)
		defer release()
		if err := p.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return p, logPath
}

const privateInitFixture = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`

func ownedIsolatedRequest(p *IsolatedStdioSessions, method, grant, sid, body string) *httptest.ResponseRecorder {
	return ownedIsolatedRequestWithContext(context.Background(), p, method, grant, sid, body)
}

func ownedIsolatedRequestWithContext(ctx context.Context, p *IsolatedStdioSessions, method, grant, sid, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, "http://127.0.0.1/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Owned-Fixture-Grant", grant)
	if sid != "" {
		r.Header.Set("Mcp-Session-Id", sid)
		r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	}
	w := httptest.NewRecorder()
	p.HTTPHandler().ServeHTTP(w, r)
	return w
}

func ownedIsolatedInit(t *testing.T, p *IsolatedStdioSessions, grant string) string {
	t.Helper()
	w := ownedIsolatedRequest(p, http.MethodPost, grant, "", privateInitFixture)
	sid := w.Header().Get("Mcp-Session-Id")
	if w.Code != http.StatusOK || sid == "" {
		t.Fatalf("initialize status=%d body=%q", w.Code, w.Body.String())
	}
	return sid
}

func ownedIsolatedRetire(t *testing.T, p *IsolatedStdioSessions, grant, sid string) StdioHostSettlement {
	t.Helper()
	holder := p.Retirement(sid)
	w := ownedIsolatedRequest(p, http.MethodDelete, grant, sid, "")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 || holder == nil {
		t.Fatalf("revoke status=%d body=%q receipt=%t", w.Code, w.Body.String(), holder != nil)
	}
	if again := ownedIsolatedRequest(p, http.MethodDelete, grant, sid, ""); again.Code != http.StatusNotFound {
		t.Fatalf("retired SID accepted: status=%d", again.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := holder.Await(ctx)
	if err != nil || !receipt.Complete() || !receipt.OriginalIdentity.Known || !receipt.RootWait.Known {
		t.Fatalf("original settlement incomplete: %+v / %v", receipt, err)
	}
	return receipt
}

func TestOwnedIsolatedStdioSessionStateAndStderr(t *testing.T) {
	diagnostics := &safeFixtureDiagnostic{}
	restore := SetDaemonDiagWriterForTest(diagnostics)
	t.Cleanup(restore)
	p, logPath := nativeIsolatedStdio(t, "cooperative", 2)
	if w := ownedIsolatedRequest(p, http.MethodPost, "", "", privateInitFixture); w.Code != http.StatusForbidden {
		t.Fatalf("unqualified initialize: status=%d", w.Code)
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", "", `{"jsonrpc":"2.0","method":"initialize"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed initialize: status=%d", w.Code)
	}
	if p.reserved != 0 || len(p.sessions) != 0 {
		t.Fatal("invalid admission acquired capacity")
	}
	a := ownedIsolatedInit(t, p, "hub")
	b := ownedIsolatedInit(t, p, "hub")
	if a == b {
		t.Fatal("shared SID")
	}
	call := `{"jsonrpc":"2.0","id":"same","method":"tools/call"}`
	for _, sid := range []string{a, b} {
		w := ownedIsolatedRequest(p, http.MethodPost, "hub", sid, call)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"count":1`) {
			t.Fatalf("private state count: status=%d body=%q", w.Code, w.Body.String())
		}
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", "", privateInitFixture); w.Code != http.StatusTooManyRequests ||
		w.Header().Get("Retry-After") != "2" || w.Header().Get("Mcp-Session-Id") != "" || !strings.Contains(w.Body.String(), "MCP_STDIO_ISOLATED_CAPACITY") {
		t.Fatalf("capacity status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	w := ownedIsolatedRequest(p, http.MethodPost, "hub", a, `{"jsonrpc":"2.0","id":"error","method":"fixture/error"}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "synth-private-error") ||
		!strings.Contains(w.Body.String(), "provider operation failed") {
		t.Fatalf("provider error not sanitized: status=%d body=%q", w.Code, w.Body.String())
	}
	first := ownedIsolatedRetire(t, p, "hub", a)
	second := ownedIsolatedRetire(t, p, "hub", b)
	for _, got := range []StdioHostSettlement{first, second} {
		if got.PrivateStderrBytes != isolatedFixtureStderrBytes || !got.PrivateStderrEOF || got.PrivateStderrReadFailed ||
			got.RootWait.ExitCode != 0 || got.NativeSettlement.Snapshot().TerminalAction != "not-requested" {
			t.Fatalf("cooperative receipt=%+v / %+v", got, got.NativeSettlement.Snapshot())
		}
	}
	if first.OriginalIdentity.PID == second.OriginalIdentity.PID {
		t.Fatal("same original process")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("private child reached stderr log sink: %v", err)
	}
	logged := diagnostics.String()
	if strings.Contains(logged, "synth") || !strings.Contains(logged, "stderr_bytes=2097152 stderr_eof=true") {
		t.Fatalf("wrong private diagnostics: %q", logged)
	}
}

func TestOwnedIsolatedStdioCancelFenceAndForcedSettlement(t *testing.T) {
	p, _ := nativeIsolatedStdio(t, "ignore-eof", 1)
	sid := ownedIsolatedInit(t, p, "hub")
	cancel := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", sid, cancel); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("cancel-first status=%d body=%q", w.Code, w.Body.String())
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call"}`); w.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled operation status=%d", w.Code)
	}
	w := ownedIsolatedRequest(p, http.MethodPost, "hub", sid, `{"jsonrpc":"2.0","id":"7","method":"tools/call"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"count":1`) {
		t.Fatalf("cancellation reached another typed operation or child: status=%d body=%q", w.Code, w.Body.String())
	}
	s := p.Retirement(sid).s
	operation := func(id string, wantWrite bool) isolatedStdioOperation {
		t.Helper()
		now := time.Now()
		deadline := now.Add(p.limits.Operation / 2)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			op := s.requests[isolatedStdioID{value: id, string: true}]
			if op != nil && (!wantWrite || op.writeReceived) {
				result := *op
				p.mu.Unlock()
				return result
			}
			p.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
		t.Fatal("fixture did not reach deterministic writer barrier")
		return isolatedStdioOperation{}
	}
	callAsync := func(body string) <-chan *httptest.ResponseRecorder {
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() { result <- ownedIsolatedRequest(p, http.MethodPost, "hub", sid, body) }()
		return result
	}
	sendCancel := func(id string) {
		t.Helper()
		w := ownedIsolatedRequest(p, http.MethodPost, "hub", sid, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%q}}`, id))
		if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
			t.Fatalf("cancel notification status=%d body=%q", w.Code, w.Body.String())
		}
	}
	// Hold the writer before the child-commit point. Cancellation must
	// complete with zero bytes even while the writer remains unavailable.
	s.written <- struct{}{}
	queued := callAsync(`{"jsonrpc":"2.0","id":"queued","method":"tools/call"}`)
	operation("queued", false)
	sendCancel("queued")
	select {
	case w := <-queued:
		if op := operation("queued", false); w.Code != http.StatusRequestTimeout || op.writeAdmitted || op.written != 0 {
			t.Fatalf("queued operation reached child: status=%d committed=%t bytes=%d", w.Code, op.writeAdmitted, op.written)
		}
	case <-time.After(p.limits.Operation / 2):
		t.Error("cancel did not unblock queued request before releasing writer")
	}
	<-s.written
	waiting := callAsync(`{"jsonrpc":"2.0","id":"active","method":"fixture/wait-for-cancel"}`)
	committed := operation("active", true)
	if !committed.writeAdmitted || committed.written == 0 || committed.writeError != nil {
		t.Fatalf("committed write unavailable: %+v", committed)
	}
	sendCancel("active")
	select {
	case w := <-waiting:
		if want := fmt.Sprintf(`"canceled":%d`, committed.childID); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) ||
			!strings.Contains(w.Body.String(), `"id":"active"`) {
			t.Fatalf("translated cancel did not preserve original/new identities: status=%d body=%q", w.Code, w.Body.String())
		}
	case <-time.After(p.limits.Operation / 2):
		t.Fatal("fixture did not receive translated cancel")
	}
	holder := p.Retirement(sid)
	if w := ownedIsolatedRequest(p, http.MethodDelete, "hub", sid, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", w.Code)
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", "", privateInitFixture); w.Code != http.StatusTooManyRequests {
		t.Fatalf("reservation released before original grace/settle: status=%d", w.Code)
	}
	ctx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	got, err := holder.Await(ctx)
	if err != nil || !got.Complete() || !got.RootWait.Known || got.NativeSettlement.Snapshot().TerminalAction != "request-succeeded" {
		t.Fatalf("forced original receipt=%+v / %v", got, err)
	}
	discovery := ownedIsolatedInit(t, p, "discovery")
	if w := ownedIsolatedRequest(p, http.MethodPost, "discovery", discovery, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`); w.Code != http.StatusForbidden {
		t.Fatalf("discovery tool access status=%d", w.Code)
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "hub", discovery, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`); w.Code != http.StatusNotFound {
		t.Fatalf("wrong grant used discovery SID: status=%d", w.Code)
	}
	if w := ownedIsolatedRequest(p, http.MethodPost, "discovery", discovery, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tools":[]`) {
		t.Fatalf("discovery list unavailable: status=%d body=%q", w.Code, w.Body.String())
	}
	ownedIsolatedRetire(t, p, "discovery", discovery)
}

func TestOwnedIsolatedStdioOriginalMemberGraceAfterEOF(t *testing.T) {
	for _, test := range []struct {
		name    string
		release bool
	}{
		{"member-exits-before-grace", true},
		{"member-overdue", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner, _ := nativeIsolatedStdio(t, "grace-wrapper", 1)
			gate := t.TempDir()
			owner.cfg.Env[isolatedFixtureGate] = gate
			owner.limits.EOFGrace = 2 * time.Second
			sid := ownedIsolatedInit(t, owner, "hub")
			originalTestBarrier(t, filepath.Join(gate, "entered"))
			holder := owner.Retirement(sid)
			if holder == nil {
				t.Fatal("no original retirement holder")
			}
			if result := ownedIsolatedRequest(owner, http.MethodDelete, "hub", sid, ""); result.Code != http.StatusNoContent {
				t.Fatalf("delete status=%d", result.Code)
			}
			native := holder.s.host.original
			grace := holder.s.graceDeadline
			wait, cancel := context.WithDeadline(context.Background(), grace)
			defer cancel()
			for _, observed := range []<-chan struct{}{native.rootDone, native.stdoutDone, native.stderrDone} {
				if err := originalJoin(wait, observed); err != nil {
					t.Fatalf("wrapper root and EOF did not precede grace: %v", err)
				}
			}
			if !native.root.Known || native.root.ExitCode != 0 || !native.stderr.eof {
				t.Fatalf("wrapper natural exit or EOF missing: root=%+v stderrEOF=%t", native.root, native.stderr.eof)
			}
			if result := ownedIsolatedRequest(owner, http.MethodPost, "hub", "", privateInitFixture); result.Code != http.StatusTooManyRequests {
				t.Fatalf("original member lost its reservation before the grace settled: status=%d", result.Code)
			}
			if test.release {
				if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Before(grace) {
					t.Fatal("test did not release member within the original grace")
				}
			}
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			result, err := holder.Await(ctx)
			if err != nil || !result.Complete() || result.NativeSettlement.Snapshot().InitialTotal < 2 {
				t.Fatalf("incomplete original Job settlement: %+v / %v", result, err)
			}
			action := result.NativeSettlement.Snapshot().TerminalAction
			_, memberMarker := os.Stat(filepath.Join(gate, "exited-cooperatively"))
			if test.release {
				if action != "not-requested" || memberMarker != nil {
					t.Fatalf("member did not finish cooperatively: action=%q marker=%v", action, memberMarker)
				}
			} else if time.Now().Before(grace) || action != "request-succeeded" || !os.IsNotExist(memberMarker) {
				t.Fatalf("overdue branch: action=%q graceEnded=%t memberMarker=%v", action, !time.Now().Before(grace), memberMarker)
			}
		})
	}
}
