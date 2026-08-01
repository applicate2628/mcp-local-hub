package vcpkgserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/vcpkgmcp/lastfailure"
)

type recordingWriteCloser struct {
	mu     sync.Mutex
	writer io.Writer
	buffer bytes.Buffer
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer.Write(p)
	return w.writer.Write(p)
}
func (*recordingWriteCloser) Close() error { return nil }
func (w *recordingWriteCloser) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...)
}

type noCloseWriter struct{ io.Writer }

func (noCloseWriter) Close() error { return nil }

func newBoundedFailureFixture(t *testing.T) (lastfailure.Args, lastfailure.Deps) {
	t.Helper()
	root := t.TempDir()
	portDir := filepath.Join(root, "wire")
	if err := os.Mkdir(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := strings.Repeat("C:\\src\\a.cpp(1,1): error C2065: bounded \\\"quoted\\\" diagnostic\n", 300)
	for i := 0; i < 20; i++ {
		name := filepath.Join(portDir, "install-cl-cfg"+strings.Repeat("x", i)+"-err.log")
		if err := os.WriteFile(name, []byte(log), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return lastfailure.Args{Port: "wire", Triplet: "cl", BuildtreesRoot: root},
		lastfailure.Deps{FS: lastfailure.DefaultFS(), Getenv: func(string) string { return "" }}
}

func callLastFailureOverRecordedSDK(t *testing.T) ([]byte, *mcp.CallToolResult) {
	t.Helper()
	fixtureArgs, fixtureDeps := newBoundedFailureFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "vcpkg-wire-test", Version: "test"}, nil)
	vs := &VcpkgServer{server: server,
		lastFailureRun: func(ctx context.Context, _ lastfailure.Args, _ lastfailure.Deps) lastfailure.Result {
			return lastfailure.LastFailureContext(ctx, fixtureArgs, fixtureDeps)
		},
		lastFailureDeps: func() lastfailure.Deps { return fixtureDeps },
	}
	registerTools(vs)
	serverConn, clientConn := net.Pipe()
	recorder := &recordingWriteCloser{writer: serverConn}
	serverTransport := &mcp.IOTransport{Reader: serverConn, Writer: recorder}
	clientTransport := &mcp.IOTransport{Reader: clientConn, Writer: noCloseWriter{clientConn}}
	ctx := t.Context()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "wire-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	before := len(recorder.bytes())
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "vcpkg_last_failure", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	delta := recorder.bytes()[before:]
	lines := bytes.Split(delta, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}
		line := append(append([]byte(nil), lines[i]...), '\n')
		return line, result
	}
	t.Fatal("SDK wrote no response line")
	return nil, nil
}

func TestVcpkgLastFailure_MCPEnvelopeStaysBelowStdioHostLineLimit(t *testing.T) {
	line, _ := callLastFailureOverRecordedSDK(t)
	if len(line) > 640<<10 || len(line) >= 1<<20 {
		t.Fatalf("outer SDK response line=%d bytes, target<=%d hard<%d", len(line), 640<<10, 1<<20)
	}
	var envelope map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(line), &envelope); err != nil {
		t.Fatalf("outer JSON-RPC: %v", err)
	}
	if envelope["jsonrpc"] != "2.0" || envelope["result"] == nil {
		t.Fatalf("not a normal JSON-RPC result: %v", envelope)
	}
}

func TestVcpkgLastFailure_MCPCallPreservesFailedCausalTuple(t *testing.T) {
	_, result := callLastFailureOverRecordedSDK(t)
	if result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("result=%#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content=%T", result.Content[0])
	}
	var body lastfailure.Result
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != lastfailure.Status("failed") || body.FirstError == nil || len(body.Diagnostics) == 0 ||
		body.Diagnostics[0] != *body.FirstError || body.DiagnosticLog == "" || len(body.LogPaths) == 0 || body.LogPaths[0] != body.DiagnosticLog {
		t.Fatalf("causal tuple lost over SDK wire: %+v", body)
	}
}

func TestVcpkgLastFailure_ConcurrentLimitIsContextAware(t *testing.T) {
	var active, maximum, starts atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	vs := &VcpkgServer{
		lastFailureRun: func(ctx context.Context, _ lastfailure.Args, _ lastfailure.Deps) lastfailure.Result {
			starts.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return lastfailure.ResourceResult(lastfailure.ReasonResourceCancelled)
			case <-release:
				return lastfailure.ResourceResult(lastfailure.ReasonNoDiagnosticFound)
			}
		},
		lastFailureDeps: func() lastfailure.Deps { return lastfailure.Deps{} },
	}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}}
	ctx1, cancel1 := context.WithCancel(t.Context())
	defer cancel1()
	resultCh := make(chan projectableToolOutcome, 4)
	call := func(ctx context.Context) { resultCh <- vs.lastFailureTool(ctx, req) }
	go call(ctx1)
	go call(t.Context())
	<-started
	<-started
	third := vs.lastFailureTool(t.Context(), req)
	if reason := lastFailureOutcomeReason(t, third); reason != lastfailure.ReasonResourceBusy {
		t.Fatalf("third reason=%q, want resource_busy", reason)
	}
	if starts.Load() != 2 {
		t.Fatalf("saturated call started filesystem work: starts=%d", starts.Load())
	}
	cancel1()
	<-resultCh
	go call(t.Context())
	<-started
	if maximum.Load() > 2 || starts.Load() != 3 {
		t.Fatalf("maximum=%d starts=%d", maximum.Load(), starts.Load())
	}
	close(release)
	<-resultCh
	<-resultCh
	if active.Load() != 0 {
		t.Fatalf("active=%d after all return paths", active.Load())
	}
}

func lastFailureOutcomeReason(t *testing.T, outcome projectableToolOutcome) lastfailure.Reason {
	t.Helper()
	if outcome.invalidArgument != nil {
		t.Fatalf("last-failure typed outcome has invalid arguments: %v", outcome.invalidArgument)
	}
	if outcome.err != nil {
		t.Fatalf("last-failure typed outcome has internal error: %v", outcome.err)
	}
	if outcome.result == nil {
		t.Fatal("last-failure typed outcome has nil projectable result")
	}
	result, ok := outcome.result.(lastfailure.Result)
	if !ok {
		t.Fatalf("last-failure typed outcome result=%T, want lastfailure.Result", outcome.result)
	}
	return result.Reason
}

func resultReason(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content=%T", result.Content[0])
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatal(err)
	}
	return body.Reason
}
