package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	hubprocess "mcp-local-hub/internal/process"
)

// fakeLSPCommand returns a portable shell command that behaves enough like a
// stdio MCP server to validate the lifecycle primitive without requiring a
// real LSP binary on CI. It reads the incoming `initialize` request, extracts
// its id, replies with a matching initialize response, then quietly swallows
// the `notifications/initialized` that follows and blocks on further stdin so
// Stop()'s tree-kill path is exercised.
//
// Matching the incoming id is important because StdioHost rewrites every
// JSON-RPC id to a monotonic internal counter — the reply must echo that
// internal id back so SendRPC's pending-map lookup succeeds.
type fakeLSPMode struct {
	receiptPath string
	handshake   string
	releasePath string
}

func fakeLSPCommand(t *testing.T, modes ...fakeLSPMode) (cmd string, args []string) {
	t.Helper()
	if len(modes) > 1 {
		t.Fatal("fakeLSPCommand accepts at most one mode")
	}
	if len(modes) == 1 {
		mode := modes[0]
		if mode.receiptPath == "" {
			t.Fatal("owned-tree mode requires a receipt path")
		}
		return "python", []string{"-u", "-c", `
import json, os, subprocess, sys, time

receipt_path = sys.argv[1]
handshake = sys.argv[2]
release_path = sys.argv[3]
child = None

def reply(message):
    request_id = message.get("id")
    if request_id is None:
        return
    if message.get("method") == "initialize":
        version = message.get("params", {}).get("protocolVersion", "2025-03-26")
        result = {"capabilities": {}, "protocolVersion": version}
    elif message.get("method") == "tools/list":
        result = {"tools": [{"name": "dummy-control"}]}
    else:
        result = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": request_id, "result": result}) + "\n")
    sys.stdout.flush()

try:
    first_line = sys.stdin.readline()
    first = json.loads(first_line)
    child = subprocess.Popen([sys.executable, "-u", "-c", "import time; time.sleep(3600)"], stdin=subprocess.DEVNULL)
    with open(receipt_path, "x", encoding="utf-8") as receipt:
        json.dump({"child_pid": child.pid, "parent_pid": os.getpid(), "ready": True}, receipt)
        receipt.flush()
        os.fsync(receipt.fileno())
    if handshake == "success":
        reply(first)
    elif handshake == "reject":
        while not os.path.exists(release_path):
            time.sleep(0.01)
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": first.get("id"), "error": {"code": -32000, "message": "injected handshake rejection"}}) + "\n")
        sys.stdout.flush()
    for line in sys.stdin:
        if handshake != "success":
            continue
        try:
            reply(json.loads(line))
        except Exception:
            continue
finally:
    if child is not None:
        try:
            child.terminate()
            child.wait(timeout=2)
        except Exception:
            try:
                child.kill()
                child.wait(timeout=2)
            except Exception:
                pass
`, mode.receiptPath, mode.handshake, mode.releasePath}
	}
	if runtime.GOOS == "windows" {
		// Windows: python is consistently on PATH for the project's test
		// suite (TestHostSubprocessLifecycle relies on it).
		return "python", []string{"-u", "-c",
			`import sys, json
# Echo back whatever id the host wrote for the initialize request.
line = sys.stdin.readline()
try:
    req = json.loads(line)
    rid = req.get("id", 1)
except Exception:
    rid = 1
sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":rid,"result":{"capabilities":{}}}) + "\n")
sys.stdout.flush()
# Drain notifications/initialized and any later traffic.
for _ in sys.stdin:
    pass
`}
	}
	return "sh", []string{"-c",
		`read -r line
rid=$(printf '%s' "$line" | python -c 'import sys,json; d=json.loads(sys.stdin.read()); print(d.get("id",1))')
printf '{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{}}}\n' "$rid"
cat`}
}

func TestBackendLifecycle_StdioMaterializeStops(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	if b.Kind() != "mcp-language-server" {
		t.Errorf("Kind = %q", b.Kind())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := b.Materialize(ctx)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if ep == nil {
		t.Fatal("endpoint nil")
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent.
	if err := b.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestBackendLifecycle_MissingBinarySurfaces(t *testing.T) {
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: "this-binary-does-not-exist-xyz-9999", Workspace: t.TempDir(),
	})
	_, err := b.Materialize(context.Background())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !IsMissingBinaryErr(err) {
		t.Errorf("error should classify as missing-binary: %v", err)
	}
}

// TestBackendLifecycle_MissingWrappedLSPClassifiedAsMissing guards the
// "wrapper installed, language server missing" case: mcp-language-server
// exists on PATH but the configured -lsp binary does not. Without the
// pre-flight LookPath check, the wrapper starts successfully and fails
// during MCP handshake, which wrapInitErr wraps as a generic init failure
// → LifecycleFailed. The contract expects LifecycleMissing for any missing
// binary in the chain, so the proxy can render the correct status cell
// and the operator sees "install pyright" not "debug init failure".
func TestBackendLifecycle_MissingWrappedLSPClassifiedAsMissing(t *testing.T) {
	wrapper, wrapperArgs := fakeLSPCommand(t) // a binary that actually exists
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: wrapper,
		WrapperArgs:    wrapperArgs,
		Workspace:      t.TempDir(),
		Language:       "python",
		LSPCommand:     "this-lsp-binary-does-not-exist-xyz-9999",
	})
	_, err := b.Materialize(context.Background())
	if err == nil {
		t.Fatal("expected error when wrapped LSP binary is missing")
	}
	if !IsMissingBinaryErr(err) {
		t.Errorf("missing LSP binary must classify as missing (not failed); got: %v", err)
	}
	if !strings.Contains(err.Error(), "this-lsp-binary-does-not-exist") {
		t.Errorf("error must name the missing LSP binary for operator clarity: %v", err)
	}
}

func TestBackendLifecycle_GoplsMissingBinarySurfaces(t *testing.T) {
	b := NewGoplsMCPStdio(GoplsMCPStdioConfig{
		WrapperCommand: "this-binary-does-not-exist-gopls-9999",
		Workspace:      t.TempDir(),
	})
	if b.Kind() != "gopls-mcp" {
		t.Errorf("Kind = %q", b.Kind())
	}
	_, err := b.Materialize(context.Background())
	if err == nil {
		t.Fatal("expected error for missing gopls binary")
	}
	if !IsMissingBinaryErr(err) {
		t.Errorf("gopls missing-binary classification: %v", err)
	}
}

// echoLSPCommand is a multi-request variant of fakeLSPCommand: it loops
// over stdin, reads each JSON-RPC request, and echoes back a response
// with the same id and an empty result. Lets tests exercise SendRequest
// on forwarded calls after the initial handshake.
func echoLSPCommand(t *testing.T) (cmd string, args []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "python", []string{"-u", "-c",
			`import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    rid = req.get("id")
    # Notifications have no id; do not reply.
    if rid is None:
        continue
    sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":rid,"result":{}}) + "\n")
    sys.stdout.flush()
`}
	}
	return "sh", []string{"-c", `while read -r line; do
  rid=$(printf '%s' "$line" | python -c 'import sys,json; d=json.loads(sys.stdin.read()); v=d.get("id"); print("" if v is None else json.dumps(v))')
  if [ -n "$rid" ]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$rid"
  fi
done`}
}

// TestBackendLifecycle_SendRequestPreservesClientID guards the JSON-RPC
// correlation contract for FORWARDED calls (not just ping): SendRPC
// multiplexes concurrent callers through an internal counter, so the
// raw backend response returns stamped with that internal id. The
// endpoint must restore the client's original id before handing the
// response back up the stack. Clients that use string ids, negative
// ids, or non-sequential concurrent ids would otherwise see apparent
// timeouts because their id-based reply matcher misses the response.
func TestBackendLifecycle_SendRequestPreservesClientID(t *testing.T) {
	cmd, args := echoLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand:   cmd,
		WrapperArgs:      args,
		Workspace:        t.TempDir(),
		Language:         "python",
		HandshakeTimeout: 3 * time.Second,
	})
	defer func() { _ = b.Stop() }()
	ep, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Use a distinctive client id that cannot collide with the internal
	// integer counter SendRPC picks — a JSON string id + a negative int.
	cases := []json.RawMessage{
		json.RawMessage(`"client-string-id-xyz"`),
		json.RawMessage(`-42`),
	}
	for _, clientID := range cases {
		req := &JSONRPCRequest{Jsonrpc: "2.0", ID: clientID, Method: "tools/list"}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := ep.SendRequest(ctx, req)
		cancel()
		if err != nil {
			t.Fatalf("SendRequest %s: %v", clientID, err)
		}
		if string(resp.ID) != string(clientID) {
			t.Errorf("response id = %s, want %s (SendRPC rewrote id for multiplex; endpoint must restore)",
				string(resp.ID), string(clientID))
		}
	}
}

func TestBackendLifecycle_SendRequestAfterStopErrors(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	ep, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	_, err = ep.SendRequest(context.Background(), &JSONRPCRequest{Method: "tools/call", ID: json.RawMessage(`1`)})
	if err == nil {
		t.Error("SendRequest after Stop must error")
	}
}

func TestBackendLifecycle_EndpointCloseIsIdempotent(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	ep, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := ep.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := ep.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// After Close the endpoint rejects further SendRequest.
	_, err = ep.SendRequest(context.Background(), &JSONRPCRequest{Method: "ping", ID: json.RawMessage(`1`)})
	if err == nil {
		t.Error("SendRequest after Close must error")
	}
}

// TestBackendLifecycle_MaterializeCompletesMCPHandshake_McpLanguageServer
// drives the real mcp-language-server.exe wrapper through Materialize and
// asserts the post-spawn MCP handshake completes within a reasonable bound.
// Skips when the binary is not on PATH — keeps CI green on machines without
// the dev toolchain. A real tools/call afterward would require the wrapper's
// LSP child (gopls) also be installed; we stop at handshake to keep the
// assertion surface focused on the regression this guards.
func TestBackendLifecycle_MaterializeCompletesMCPHandshake_McpLanguageServer(t *testing.T) {
	bin := "mcp-language-server"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping live probe", bin)
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skipf("gopls (required by mcp-language-server wrapper) not on PATH; skipping live probe")
	}
	ws := t.TempDir()
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand:   bin,
		WrapperArgs:      []string{"-workspace", ws, "-lsp", "gopls"},
		Workspace:        ws,
		HandshakeTimeout: 15 * time.Second,
	})
	defer b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	ep, err := b.Materialize(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Materialize against real %s failed after %s: %v", bin, elapsed, err)
	}
	if ep == nil {
		t.Fatal("endpoint nil")
	}
	t.Logf("%s Materialize+handshake completed in %s", bin, elapsed)
}

// TestBackendLifecycle_MaterializeCompletesMCPHandshake_GoplsMCP is the
// equivalent live probe for `gopls mcp`.
func TestBackendLifecycle_MaterializeCompletesMCPHandshake_GoplsMCP(t *testing.T) {
	bin := "gopls"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping live probe", bin)
	}
	ws := t.TempDir()
	b := NewGoplsMCPStdio(GoplsMCPStdioConfig{
		WrapperCommand:   bin,
		Workspace:        ws,
		HandshakeTimeout: 15 * time.Second,
	})
	defer b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	ep, err := b.Materialize(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Materialize against real gopls mcp failed after %s: %v", elapsed, err)
	}
	if ep == nil {
		t.Fatal("endpoint nil")
	}
	t.Logf("gopls mcp Materialize+handshake completed in %s", elapsed)
}

// TestBackendLifecycle_HandshakeTimeoutTearsDownSubprocess verifies the
// handshake timeout path: a backend that never answers initialize must cause
// Materialize to tear the subprocess down and return a wrapped error (not
// hang forever).
func TestBackendLifecycle_HandshakeTimeoutTearsDownSubprocess(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "python"
		args = []string{"-u", "-c", `import sys
# Silent backend: never replies, just drains stdin.
for _ in sys.stdin:
    pass
`}
	} else {
		cmd = "sh"
		args = []string{"-c", "cat >/dev/null"}
	}
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand:   cmd,
		WrapperArgs:      args,
		Workspace:        t.TempDir(),
		HandshakeTimeout: 500 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := b.Materialize(ctx)
	if err == nil {
		t.Fatal("Materialize must error when handshake times out")
	}
	if IsMissingBinaryErr(err) {
		t.Errorf("timeout error must NOT classify as missing-binary: %v", err)
	}
	// Second Materialize after teardown should spawn fresh and again time
	// out — verifies the host was cleaned up rather than left dangling.
	_, err2 := b.Materialize(ctx)
	if err2 == nil {
		t.Error("second Materialize after tear-down must also error")
	}
}

func TestBackendLifecycle_MaterializeReturnsExistingEndpoint(t *testing.T) {
	cmd, args := fakeLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
	})
	defer b.Stop()
	ep1, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ep2, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep1 == nil || ep2 == nil {
		t.Fatal("endpoints must be non-nil on both Materialize calls")
	}
}

// TestBackendLifecycle_SendRPCPendingCapRefusesPreDelivery covers
// work-items/bugs/2026-07-02-sendrpc-pending-uncapped.md: SendRPC must
// enforce the same maxPendingRequests bound handlePOST does — the
// await-after-delivery model holds first cold requests longer, so a fan-out
// spike on a cold backend would otherwise grow h.pending without bound on
// this path. The refusal fires BEFORE the stdin write (pre-delivery), so a
// caller can safely retry once slots drain — proven by the drain-then-retry
// half of the test.
func TestBackendLifecycle_SendRPCPendingCapRefusesPreDelivery(t *testing.T) {
	// echoLSPCommand (not fakeLSPCommand): the drain-retry half needs a fake
	// that actually answers tools/list, mirroring SendRequestPreservesClientID.
	cmd, args := echoLSPCommand(t)
	b := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand: cmd, WrapperArgs: args, Workspace: t.TempDir(),
		Language: "python", HandshakeTimeout: 3 * time.Second,
	})
	defer func() { _ = b.Stop() }()
	ep, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	host := ep.(*stdioHostEndpoint).host

	// Park synthetic pending entries up to the cap (keys far above the
	// internal counter so they can't collide with the real request below).
	host.pendingMu.Lock()
	for i := 0; i < maxPendingRequests; i++ {
		host.pending[int64(1_000_000+i)] = make(chan json.RawMessage, 1)
	}
	host.pendingMu.Unlock()
	defer func() {
		host.pendingMu.Lock()
		for i := 0; i < maxPendingRequests; i++ {
			delete(host.pending, int64(1_000_000+i))
		}
		host.pendingMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = host.SendRPC(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err == nil {
		t.Fatal("SendRPC at the pending cap must refuse")
	}
	if !strings.Contains(err.Error(), "too many pending requests") {
		t.Fatalf("error = %v, want the too-many-pending refusal", err)
	}

	// Drain one slot → the retry must go through (pre-delivery refusal is
	// retry-safe; nothing was written to the backend on the refused call).
	host.pendingMu.Lock()
	delete(host.pending, int64(1_000_000))
	host.pendingMu.Unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := host.SendRPC(ctx2, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != nil {
		t.Fatalf("SendRPC after draining one slot: %v", err)
	}
}

func TestBackendLifecycle_DummyOwnedResourcesSettle(t *testing.T) {
	t.Run("shared sessions and owner shutdown", func(t *testing.T) {
		control, controlReceiptPath, _ := newOwnedDummyLifecycle(t, "success")
		var controlReceipt *dummyOwnedTreeReceipt
		t.Cleanup(func() {
			cleanupDummyOwnedResources([]*mcpLanguageServerStdio{control}, []*dummyOwnedTreeReceipt{controlReceipt})
		})
		controlEndpoint, controlHost, controlReceipt := materializeOwnedDummy(t, control, controlReceiptPath)
		controlBefore := dummyControlResponse(t, controlEndpoint)

		target, targetReceiptPath, _ := newOwnedDummyLifecycle(t, "success")
		var targetReceipt *dummyOwnedTreeReceipt
		t.Cleanup(func() {
			cleanupDummyOwnedResources([]*mcpLanguageServerStdio{target}, []*dummyOwnedTreeReceipt{targetReceipt})
		})
		targetEndpointA, targetHost, targetReceipt := materializeOwnedDummy(t, target, targetReceiptPath)
		targetEndpointB, err := target.Materialize(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if targetEndpointB.(*stdioHostEndpoint).host != targetHost || target.host != targetHost {
			t.Fatal("backing-owner reference changed after repeated materialization")
		}
		if targetHost.cmd == nil || targetHost.cmd.Process == nil || targetHost.cmd.Process.Pid != targetReceipt.ParentPID {
			t.Fatalf("target parent identity host=%v receipt=%+v", targetHost.cmd, targetReceipt)
		}
		assertDummyReceiptLive(t, targetReceipt)
		assertDummyReceiptLive(t, controlReceipt)
		if runtime.GOOS == "windows" && targetHost.job == nil {
			t.Fatal("handle owner StdioHost.job is nil after the receipt-bound child spawned")
		}

		server := httptest.NewServer(targetHost.HTTPHandler())
		t.Cleanup(server.Close)
		listenerAddress := server.Listener.Addr().String()
		initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"p5-dummy","version":"1"}}}`
		initializeResults := make(chan dummyHTTPResult, 2)
		startInitializes := make(chan struct{})
		for range 2 {
			go func() {
				<-startInitializes
				initializeResults <- dummyInitializeRequest(server.Client(), server.URL, initBody)
			}()
		}
		close(startInitializes)
		first := <-initializeResults
		second := <-initializeResults
		if first.err != nil || second.err != nil {
			t.Fatalf("concurrent initialize errors=(%v,%v)", first.err, second.err)
		}
		if first.status != http.StatusOK || second.status != http.StatusOK {
			t.Fatalf("initialize statuses=(%d,%d), want 200/200", first.status, second.status)
		}
		sessionA := first.header.Get("Mcp-Session-Id")
		sessionB := second.header.Get("Mcp-Session-Id")
		if sessionA == "" || sessionB == "" || sessionA == sessionB {
			t.Fatalf("HTTP session references=(%q,%q), want two distinct IDs", sessionA, sessionB)
		}
		if got := dummyHostSessionCount(targetHost); got != 2 {
			t.Fatalf("HTTP session references StdioHost.sessions=%d, want 2", got)
		}
		if issues := dummyHostInflightFailures(targetHost); len(issues) != 0 {
			t.Fatalf("live target first nonzero class: %s; all=%v", issues[0], issues)
		}

		callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
		beforeDelete := sessionContractRequest(t, server.Client(), http.MethodPost, server.URL, callBody, sessionB, "2025-11-25")
		if beforeDelete.status != http.StatusOK || !strings.Contains(string(beforeDelete.body), "dummy-control") {
			t.Fatalf("session B control before DELETE status=%d body=%s", beforeDelete.status, beforeDelete.body)
		}
		deleted := sessionContractRequest(t, server.Client(), http.MethodDelete, server.URL, "", sessionA, "2025-11-25")
		if deleted.status != http.StatusNoContent {
			t.Fatalf("DELETE A status=%d want=204 body=%s", deleted.status, deleted.body)
		}
		deletedCall := sessionContractRequest(t, server.Client(), http.MethodPost, server.URL, callBody, sessionA, "2025-11-25")
		if deletedCall.status != http.StatusNotFound {
			t.Fatalf("deleted session A status=%d want=404 body=%s", deletedCall.status, deletedCall.body)
		}
		afterDelete := sessionContractRequest(t, server.Client(), http.MethodPost, server.URL, callBody, sessionB, "2025-11-25")
		if afterDelete.status != http.StatusOK || string(afterDelete.body) != string(beforeDelete.body) {
			t.Fatalf("session B changed across DELETE A: before=%s after=(%d)%s", beforeDelete.body, afterDelete.status, afterDelete.body)
		}
		if got := dummyHostSessionCount(targetHost); got != 1 {
			t.Fatalf("HTTP session references StdioHost.sessions=%d after DELETE A, want 1", got)
		}
		assertDummyReceiptLive(t, targetReceipt)
		assertDummyReceiptLive(t, controlReceipt)
		if got := dummyControlResponse(t, controlEndpoint); got != controlBefore {
			t.Fatalf("control response changed after DELETE A: before=%s after=%s", controlBefore, got)
		}

		if err := targetEndpointA.Close(); err != nil {
			t.Fatal(err)
		}
		if err := targetEndpointB.Close(); err != nil {
			t.Fatal(err)
		}
		var targetIssues []string
		if err := target.Stop(); err != nil {
			targetIssues = append(targetIssues, "process/tree StdioHost.Stop error="+err.Error())
		}
		if err := target.Stop(); err != nil {
			targetIssues = append(targetIssues, "backing-owner repeated Stop error="+err.Error())
		}
		lateInitialize := dummyInitializeRequest(server.Client(), server.URL, initBody)
		if lateInitialize.err != nil {
			targetIssues = append(targetIssues, "cached initialize after Stop transport error="+lateInitialize.err.Error())
		} else if lateInitialize.status != http.StatusInternalServerError {
			targetIssues = append(targetIssues, fmt.Sprintf("cached initialize after Stop status=%d want=500", lateInitialize.status))
		}
		server.Close()
		rebound, err := net.Listen("tcp", listenerAddress)
		if err != nil {
			targetIssues = append(targetIssues, "port listener rebind failed="+err.Error())
		} else {
			_ = rebound.Close()
		}
		if _, err := targetEndpointB.SendRequest(context.Background(), &JSONRPCRequest{Jsonrpc: "2.0", ID: json.RawMessage(`3`), Method: "tools/list", Params: json.RawMessage(`{}`)}); err == nil {
			targetIssues = append(targetIssues, "handle endpoint accepted post-stop pipe use")
		}
		targetIssues = append(targetIssues, dummyHostSettledFailures(target, targetHost, targetReceipt)...)

		assertDummyReceiptLive(t, controlReceipt)
		if got := dummyControlResponse(t, controlEndpoint); got != controlBefore {
			t.Fatalf("control response changed after target shutdown: before=%s after=%s", controlBefore, got)
		}
		var controlIssues []string
		if err := controlEndpoint.Close(); err != nil {
			controlIssues = append(controlIssues, "control endpoint close error="+err.Error())
		}
		if err := control.Stop(); err != nil {
			controlIssues = append(controlIssues, "control process/tree Stop error="+err.Error())
		}
		controlIssues = append(controlIssues, dummyHostSettledFailures(control, controlHost, controlReceipt)...)
		if len(controlIssues) != 0 {
			t.Fatalf("control cleanup first nonzero class: %s; all=%v", controlIssues[0], controlIssues)
		}
		if len(targetIssues) != 0 {
			t.Fatalf("target cleanup first nonzero class: %s; all=%v", targetIssues[0], targetIssues)
		}
	})

	t.Run("materialize cancellation", func(t *testing.T) {
		runDummyMaterializeFailureScenario(t, "withhold", true)
	})

	t.Run("handshake failure", func(t *testing.T) {
		runDummyMaterializeFailureScenario(t, "reject", false)
	})
}

type dummyOwnedTreeReceipt struct {
	ChildPID  int  `json:"child_pid"`
	ParentPID int  `json:"parent_pid"`
	Ready     bool `json:"ready"`
	child     *os.Process
	parent    *os.Process
}

type dummyMaterializeResult struct {
	endpoint MCPEndpoint
	err      error
}

type dummyHTTPResult struct {
	status int
	header http.Header
	err    error
}

func newOwnedDummyLifecycle(t *testing.T, handshake string) (*mcpLanguageServerStdio, string, string) {
	t.Helper()
	root := t.TempDir()
	receiptPath := filepath.Join(root, "owned-tree.json")
	releasePath := filepath.Join(root, "release-handshake")
	cmd, args := fakeLSPCommand(t, fakeLSPMode{receiptPath: receiptPath, handshake: handshake, releasePath: releasePath})
	lifecycle := NewMcpLanguageServerStdio(McpLanguageServerStdioConfig{
		WrapperCommand:   cmd,
		WrapperArgs:      args,
		Workspace:        t.TempDir(),
		HandshakeTimeout: 3 * time.Second,
	}).(*mcpLanguageServerStdio)
	return lifecycle, receiptPath, releasePath
}

func materializeOwnedDummy(t *testing.T, lifecycle *mcpLanguageServerStdio, receiptPath string) (MCPEndpoint, *StdioHost, *dummyOwnedTreeReceipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, err := lifecycle.Materialize(ctx)
	if err != nil {
		t.Fatalf("Materialize DUMMY: %v", err)
	}
	receipt := readDummyOwnedTreeReceipt(t, receiptPath)
	host := endpoint.(*stdioHostEndpoint).host
	if host == nil {
		t.Fatal("materialized endpoint has no StdioHost")
	}
	return endpoint, host, receipt
}

func runDummyMaterializeFailureScenario(t *testing.T, handshake string, cancelAfterReceipt bool) {
	t.Helper()
	control, controlReceiptPath, _ := newOwnedDummyLifecycle(t, "success")
	var controlReceipt *dummyOwnedTreeReceipt
	t.Cleanup(func() {
		cleanupDummyOwnedResources([]*mcpLanguageServerStdio{control}, []*dummyOwnedTreeReceipt{controlReceipt})
	})
	controlEndpoint, controlHost, controlReceipt := materializeOwnedDummy(t, control, controlReceiptPath)
	controlBefore := dummyControlResponse(t, controlEndpoint)

	target, targetReceiptPath, targetReleasePath := newOwnedDummyLifecycle(t, handshake)
	var targetReceipt *dummyOwnedTreeReceipt
	t.Cleanup(func() {
		cleanupDummyOwnedResources([]*mcpLanguageServerStdio{target}, []*dummyOwnedTreeReceipt{targetReceipt})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan dummyMaterializeResult, 1)
	go func() {
		endpoint, err := target.Materialize(ctx)
		done <- dummyMaterializeResult{endpoint: endpoint, err: err}
	}()
	targetReceipt = readDummyOwnedTreeReceipt(t, targetReceiptPath)
	assertDummyReceiptLive(t, targetReceipt)
	if cancelAfterReceipt {
		cancel()
	} else if err := os.WriteFile(targetReleasePath, []byte("release"), 0o600); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer cancel()

	var result dummyMaterializeResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Materialize did not settle after test-owned release")
	}
	if result.endpoint != nil || result.err == nil {
		t.Fatalf("failed Materialize result=(%v,%v), want nil endpoint and error", result.endpoint, result.err)
	}
	if cancelAfterReceipt {
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("canceled Materialize error=%v, want context.Canceled", result.err)
		}
	} else if !strings.Contains(result.err.Error(), "injected handshake rejection") {
		t.Fatalf("rejected Materialize error=%v, want injected handshake cause", result.err)
	}
	target.mu.Lock()
	targetHost := target.host
	target.mu.Unlock()
	var targetIssues []string
	if targetHost != nil {
		targetIssues = append(targetIssues, "backing-owner reference mcpLanguageServerStdio.host remained published")
	}
	if !waitDummyReceiptStopped(targetReceipt, 3*time.Second) {
		targetIssues = append(targetIssues, fmt.Sprintf("process/tree receipt remains live parent=%d child=%d", targetReceipt.ParentPID, targetReceipt.ChildPID))
	}
	if err := target.Stop(); err != nil {
		targetIssues = append(targetIssues, "failed lifecycle repeated Stop error="+err.Error())
	}

	assertDummyReceiptLive(t, controlReceipt)
	if got := dummyControlResponse(t, controlEndpoint); got != controlBefore {
		t.Fatalf("control response changed after target failure: before=%s after=%s", controlBefore, got)
	}
	var controlIssues []string
	if err := controlEndpoint.Close(); err != nil {
		controlIssues = append(controlIssues, "control endpoint close error="+err.Error())
	}
	if err := control.Stop(); err != nil {
		controlIssues = append(controlIssues, "control process/tree Stop error="+err.Error())
	}
	controlIssues = append(controlIssues, dummyHostSettledFailures(control, controlHost, controlReceipt)...)
	if len(controlIssues) != 0 {
		t.Fatalf("control cleanup first nonzero class: %s; all=%v", controlIssues[0], controlIssues)
	}
	if len(targetIssues) != 0 {
		t.Fatalf("failed target first nonzero class: %s; all=%v", targetIssues[0], targetIssues)
	}
}

func readDummyOwnedTreeReceipt(t *testing.T, path string) *dummyOwnedTreeReceipt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	var err error
	for time.Now().Before(deadline) {
		body, err = os.ReadFile(path)
		if err == nil {
			var fields map[string]json.RawMessage
			if json.Unmarshal(body, &fields) == nil && len(fields) == 3 && fields["child_pid"] != nil && fields["parent_pid"] != nil && fields["ready"] != nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read owned-tree receipt: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 4096 {
		t.Fatalf("owned-tree receipt is not a bounded regular file: info=%v err=%v", info, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 3 || fields["child_pid"] == nil || fields["parent_pid"] == nil || fields["ready"] == nil {
		t.Fatalf("owned-tree receipt shape=%s err=%v", body, err)
	}
	var receipt dummyOwnedTreeReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ChildPID <= 0 || receipt.ParentPID <= 0 || receipt.ChildPID == receipt.ParentPID || !receipt.Ready {
		t.Fatalf("owned-tree receipt values=%+v", receipt)
	}
	receipt.parent, err = os.FindProcess(receipt.ParentPID)
	if err != nil {
		t.Fatalf("bind parent PID %d: %v", receipt.ParentPID, err)
	}
	receipt.child, err = os.FindProcess(receipt.ChildPID)
	if err != nil {
		_ = receipt.parent.Release()
		t.Fatalf("bind child PID %d: %v", receipt.ChildPID, err)
	}
	return &receipt
}

func dummyControlResponse(t *testing.T, endpoint MCPEndpoint) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := endpoint.SendRequest(ctx, &JSONRPCRequest{Jsonrpc: "2.0", ID: json.RawMessage(`"p5-control"`), Method: "tools/list", Params: json.RawMessage(`{}`)})
	if err != nil || response == nil || response.Error != nil || !strings.Contains(string(response.Result), "dummy-control") {
		t.Fatalf("control response=(%+v,%v)", response, err)
	}
	return string(response.Result)
}

func dummyInitializeRequest(client *http.Client, serverURL, body string) dummyHTTPResult {
	request, err := http.NewRequest(http.MethodPost, serverURL+"/mcp", strings.NewReader(body))
	if err != nil {
		return dummyHTTPResult{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		return dummyHTTPResult{err: err}
	}
	defer response.Body.Close()
	_, err = io.ReadAll(response.Body)
	return dummyHTTPResult{status: response.StatusCode, header: response.Header.Clone(), err: err}
}

func assertDummyReceiptLive(t *testing.T, receipt *dummyOwnedTreeReceipt) {
	t.Helper()
	if !hubprocess.IsPidAlive(receipt.ParentPID) || !hubprocess.IsPidAlive(receipt.ChildPID) {
		t.Fatalf("receipt-bound tree is not live parent=%d child=%d", receipt.ParentPID, receipt.ChildPID)
	}
}

func dummyHostSessionCount(host *StdioHost) int {
	host.sessionMu.Lock()
	defer host.sessionMu.Unlock()
	return len(host.sessions)
}

func dummyHostInflightFailures(host *StdioHost) []string {
	var failures []string
	host.pendingMu.Lock()
	pending := len(host.pending)
	host.pendingMu.Unlock()
	if pending != 0 {
		failures = append(failures, fmt.Sprintf("in-flight host reference StdioHost.pending=%d", pending))
	}
	host.sseMu.Lock()
	sseClients := len(host.sseClients)
	host.sseMu.Unlock()
	if sseClients != 0 || host.sseActive.Load() != 0 {
		failures = append(failures, fmt.Sprintf("in-flight host references sseClients=%d sseActive=%d", sseClients, host.sseActive.Load()))
	}
	return failures
}

func dummyHostSettledFailures(lifecycle *mcpLanguageServerStdio, host *StdioHost, receipt *dummyOwnedTreeReceipt) []string {
	var failures []string
	if sessions := dummyHostSessionCount(host); sessions != 0 {
		failures = append(failures, fmt.Sprintf("HTTP session references StdioHost.sessions=%d", sessions))
	}
	failures = append(failures, dummyHostInflightFailures(host)...)
	lifecycle.mu.Lock()
	publishedHost := lifecycle.host
	lifecycle.mu.Unlock()
	if publishedHost != nil {
		failures = append(failures, "backing-owner reference mcpLanguageServerStdio.host is non-nil")
	}
	host.mu.Lock()
	stopped := host.stopped
	job := host.job
	logFile := host.logFile
	host.mu.Unlock()
	if !stopped {
		failures = append(failures, "handle/lifecycle StdioHost.stopped=false")
	}
	if job != nil {
		failures = append(failures, "handle owner StdioHost.job is non-nil")
	}
	if logFile != nil {
		failures = append(failures, "handle owner StdioHost.logFile is non-nil")
	}
	for name, channel := range map[string]<-chan struct{}{"done": host.done, "procExited": host.procExited, "childExited": host.childExited} {
		select {
		case <-channel:
		default:
			failures = append(failures, "lifecycle channel StdioHost."+name+" remains open")
		}
	}
	if !waitDummyReceiptStopped(receipt, 3*time.Second) {
		failures = append(failures, fmt.Sprintf("process/tree receipt remains live parent=%d child=%d", receipt.ParentPID, receipt.ChildPID))
	}
	return failures
}

func waitDummyReceiptStopped(receipt *dummyOwnedTreeReceipt, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !hubprocess.IsPidAlive(receipt.ParentPID) && !hubprocess.IsPidAlive(receipt.ChildPID) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !hubprocess.IsPidAlive(receipt.ParentPID) && !hubprocess.IsPidAlive(receipt.ChildPID)
}

func cleanupDummyOwnedResources(lifecycles []*mcpLanguageServerStdio, receipts []*dummyOwnedTreeReceipt) {
	for _, lifecycle := range lifecycles {
		if lifecycle != nil {
			_ = lifecycle.Stop()
		}
	}
	for _, receipt := range receipts {
		if receipt == nil {
			continue
		}
		if receipt.child != nil {
			if hubprocess.IsPidAlive(receipt.ChildPID) {
				_ = receipt.child.Kill()
			}
			_ = receipt.child.Release()
		}
		if receipt.parent != nil {
			if hubprocess.IsPidAlive(receipt.ParentPID) {
				_ = receipt.parent.Kill()
			}
			_ = receipt.parent.Release()
		}
	}
}
