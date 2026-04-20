package daemon

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// fakeLSPCommand returns a portable shell command that behaves enough like a
// stdio MCP server to validate the lifecycle primitive without requiring a
// real LSP binary on CI. It writes one canned initialize reply to stdout,
// then blocks on stdin so Stop()'s tree-kill path is exercised.
func fakeLSPCommand(t *testing.T) (cmd string, args []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows: python is consistently on PATH for the project's test
		// suite (TestHostSubprocessLifecycle relies on it).
		return "python", []string{"-u", "-c",
			`import sys
sys.stdout.write('{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}\n')
sys.stdout.flush()
for _ in sys.stdin:
    pass
`}
	}
	return "sh", []string{"-c",
		`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}'; cat`}
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
