package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMCPServer is a minimal Streamable-HTTP MCP server that the relay
// can talk to in tests. Caller configures behavior via callbacks.
type fakeMCPServer struct {
	t             *testing.T
	mintSessionID string
	onPOST        func(body []byte, sid string) (status int, contentType string, responseBody []byte)
	sseHoldUntil  chan struct{}
	sseEvents     []string // pre-canned events delivered on GET /mcp
	mu            sync.Mutex
	postCount     atomic.Int32
	deleteCount   atomic.Int32
	getCount      atomic.Int32
}

func (f *fakeMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			f.postCount.Add(1)
			body, _ := io.ReadAll(r.Body)
			sid := r.Header.Get("MCP-Session-Id")
			if f.postCount.Load() == 1 && f.mintSessionID != "" {
				w.Header().Set("MCP-Session-Id", f.mintSessionID)
			}
			status, ct, respBody := 202, "", []byte(nil)
			if f.onPOST != nil {
				status, ct, respBody = f.onPOST(body, sid)
			}
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(status)
			if len(respBody) > 0 {
				_, _ = w.Write(respBody)
			}
		case "GET":
			f.getCount.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			f.mu.Lock()
			events := append([]string(nil), f.sseEvents...)
			hold := f.sseHoldUntil
			f.mu.Unlock()
			for _, e := range events {
				fmt.Fprint(w, e)
				if flusher != nil {
					flusher.Flush()
				}
			}
			if hold != nil {
				select {
				case <-hold:
				case <-r.Context().Done():
				}
			}
		case "DELETE":
			f.deleteCount.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func newFakeServer(t *testing.T) *fakeMCPServer {
	return &fakeMCPServer{t: t, mintSessionID: "test-session-123", sseHoldUntil: make(chan struct{})}
}

// runRelayUntilIdle runs the relay in a goroutine, then waits briefly
// for activity to settle before canceling and returning collected
// stdout/stderr.
func runRelayUntilIdle(t *testing.T, srv *httptest.Server, stdin string, idleMs int) (stdout, stderr string) {
	t.Helper()
	stdinR := strings.NewReader(stdin)
	var stdoutW, stderrW bytes.Buffer

	r := &HTTPToStdioRelay{
		URL:    srv.URL,
		Stdin:  stdinR,
		Stdout: &stdoutW,
		Stderr: &stderrW,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for stdin drain + processing.
	time.Sleep(time.Duration(idleMs) * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit within 3s of cancellation")
	}
	return stdoutW.String(), stderrW.String()
}

// TestRelay_InitializePOSTMintsSession verifies that the relay:
//   - Sends the initialize POST with correct headers (Accept, no session on first).
//   - Captures MCP-Session-Id from the server's response header.
//   - Writes the response body line-by-line to stdout.
func TestRelay_InitializePOSTMintsSession(t *testing.T) {
	fake := newFakeServer(t)
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		// First POST has no session header.
		if sid != "" {
			return 500, "", []byte(`{"error":"first POST should have no session"}`)
		}
		return 200, "application/json",
			[]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
	stdout, stderr := runRelayUntilIdle(t, srv, stdin, 200)

	if !strings.Contains(stdout, `"result":{"protocolVersion":"2025-11-25"}`) {
		t.Errorf("stdout missing initialize result: %q (stderr: %q)", stdout, stderr)
	}
	if fake.postCount.Load() == 0 {
		t.Error("server received no POST")
	}
}

// TestRelay_NotificationGets202NoReply covers the "no reply expected"
// path: client sends a notification (no id), server returns 202 Accepted
// with empty body, relay writes nothing to stdout.
func TestRelay_NotificationGets202NoReply(t *testing.T) {
	fake := newFakeServer(t)
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		return 202, "", nil
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	stdout, _ := runRelayUntilIdle(t, srv, stdin, 150)

	if stdout != "" {
		t.Errorf("stdout should be empty for 202-response notification, got: %q", stdout)
	}
}

// TestRelay_SessionHeaderEchoedOnSubsequentPOSTs ensures that once the
// server mints MCP-Session-Id on the first response, all subsequent
// POSTs include it.
func TestRelay_SessionHeaderEchoedOnSubsequentPOSTs(t *testing.T) {
	fake := newFakeServer(t)
	var observed []string
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		observed = append(observed, sid)
		if fake.postCount.Load() == 1 {
			return 200, "application/json",
				[]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
		return 202, "", nil
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	runRelayUntilIdle(t, srv, stdin, 300)

	if len(observed) < 2 {
		t.Fatalf("expected 2+ POSTs, got %d", len(observed))
	}
	if observed[0] != "" {
		t.Errorf("first POST should have empty session header, got %q", observed[0])
	}
	if observed[1] != "test-session-123" {
		t.Errorf("second POST should echo minted session ID, got %q", observed[1])
	}
}

// TestRelay_SSEResponseOnPOST covers the Streamable HTTP variant where
// POST /mcp returns an SSE stream (multi-event response) instead of a
// single JSON body. All events' `data:` payloads must end up on stdout
// as separate lines.
func TestRelay_SSEResponseOnPOST(t *testing.T) {
	fake := newFakeServer(t)
	sseBody := "data: " + `{"jsonrpc":"2.0","method":"notifications/progress","params":{"step":1}}` + "\n\n" +
		"data: " + `{"jsonrpc":"2.0","id":1,"result":{"done":true}}` + "\n\n"
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		return 200, "text/event-stream", []byte(sseBody)
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","id":1,"method":"tools/call"}` + "\n"
	stdout, _ := runRelayUntilIdle(t, srv, stdin, 300)

	if !strings.Contains(stdout, `"method":"notifications/progress"`) {
		t.Errorf("stdout missing progress notification from SSE stream: %q", stdout)
	}
	if !strings.Contains(stdout, `"result":{"done":true}`) {
		t.Errorf("stdout missing final result from SSE stream: %q", stdout)
	}
	// Each event must land as its own line (no embedded newline, no merge).
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 stdout lines, got %d: %q", len(lines), stdout)
	}
}

// TestRelay_HTTPErrorSynthesizesJSONRPCError ensures the relay does not
// hang the client when POST fails: a JSON-RPC error response with the
// original request's id is emitted to stdout.
func TestRelay_HTTPErrorSynthesizesJSONRPCError(t *testing.T) {
	fake := newFakeServer(t)
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		return 500, "application/json", []byte(`{"error":"boom"}`)
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","id":42,"method":"tools/call"}` + "\n"
	stdout, _ := runRelayUntilIdle(t, srv, stdin, 200)

	var parsed map[string]any
	line := strings.TrimSpace(strings.Split(stdout, "\n")[0])
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("stdout first line not JSON: %q (err: %v)", line, err)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("synthetic response must have jsonrpc=2.0, got %v", parsed["jsonrpc"])
	}
	if fmt.Sprintf("%v", parsed["id"]) != "42" {
		t.Errorf("synthetic error must preserve original id=42, got %v", parsed["id"])
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("synthetic response missing error object: %v", parsed)
	}
	if errObj["code"].(float64) != -32603 {
		t.Errorf("synthetic error code = %v, want -32603", errObj["code"])
	}
}

// TestRelay_GracefulShutdownSendsDELETE verifies that when the relay
// exits cleanly (stdin EOF), it issues DELETE /mcp with the session
// header so the server can free resources.
func TestRelay_GracefulShutdownSendsDELETE(t *testing.T) {
	fake := newFakeServer(t)
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		if fake.postCount.Load() == 1 {
			return 200, "application/json",
				[]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
		return 202, "", nil
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
	runRelayUntilIdle(t, srv, stdin, 300)

	if fake.deleteCount.Load() == 0 {
		t.Error("expected DELETE /mcp on shutdown, got 0")
	}
}

func TestReadJSONRPCLine_TooLong(t *testing.T) {
	input := append(bytes.Repeat([]byte("a"), maxStdinLineBytes+1), '\n')
	br := bufio.NewReaderSize(bytes.NewReader(input), maxStdinLineBytes)

	_, err := readJSONRPCLine(br)
	if !errors.Is(err, errStdinLineTooLong) {
		t.Fatalf("expected errStdinLineTooLong, got %v", err)
	}
}

func TestReadJSONRPCLine_MaxLengthAccepted(t *testing.T) {
	input := append(bytes.Repeat([]byte("a"), maxStdinLineBytes-1), '\n')
	br := bufio.NewReaderSize(bytes.NewReader(input), maxStdinLineBytes)

	line, err := readJSONRPCLine(br)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(line) != maxStdinLineBytes-1 {
		t.Fatalf("line length = %d, want %d", len(line), maxStdinLineBytes-1)
	}
}

func TestRelay_StdinPumpCapsConcurrentPOSTs(t *testing.T) {
	fake := newFakeServer(t)
	blockPOST := make(chan struct{})
	fake.onPOST = func(body []byte, sid string) (int, string, []byte) {
		<-blockPOST
		return 202, "", nil
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	var stdin bytes.Buffer
	for i := 0; i < maxConcurrentPOSTs*3; i++ {
		fmt.Fprintf(&stdin, `{"jsonrpc":"2.0","method":"notifications/ping","params":{"n":%d}}`+"\n", i)
	}

	r := &HTTPToStdioRelay{
		URL:        srv.URL,
		Stdin:      &stdin,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	r.setSessionID("already-initialized")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	done := make(chan error, 1)
	go func() {
		done <- r.stdinPump(ctx, &wg)
	}()

	time.Sleep(200 * time.Millisecond)
	inFlight := int(fake.postCount.Load())
	if inFlight > maxConcurrentPOSTs {
		t.Fatalf("concurrent POSTs exceeded cap: got %d, cap %d", inFlight, maxConcurrentPOSTs)
	}

	close(blockPOST)
	err := <-done
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stdinPump returned %v, want io.EOF", err)
	}
	wg.Wait()
}
