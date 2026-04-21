package daemon

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
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
func fakeLSPCommand(t *testing.T) (cmd string, args []string) {
	t.Helper()
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
