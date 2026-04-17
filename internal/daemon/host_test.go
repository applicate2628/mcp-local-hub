package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHostSubprocessLifecycle verifies the stdio-host can spawn a subprocess,
// forward a write to its stdin, and capture the matching line from stdout.
// Uses a tiny echo-subprocess (writes each stdin line unchanged to stdout).
func TestHostSubprocessLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: echoSubprocCommand(),
		Args:    echoSubprocArgs(),
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	// Write a line to stdin. The echo subprocess will write it back on stdout.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if err := h.writeStdin(line); err != nil {
		t.Fatalf("writeStdin: %v", err)
	}

	// Read the next stdout line (the test-only path exposed for testing).
	got, err := h.readStdoutTest(time.Second)
	if err != nil {
		t.Fatalf("readStdoutTest: %v", err)
	}
	if !bytes.Equal(got, line) {
		t.Errorf("stdout echo: want %q, got %q", line, got)
	}

	// Sanity: the line is valid JSON-RPC.
	var msg map[string]any
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("invalid JSON in stdout: %v", err)
	}
}

// echoSubprocCommand returns a command that reads lines from stdin
// and writes each line back to stdout verbatim. Used only in tests.
func echoSubprocCommand() string {
	return "python"
}

func echoSubprocArgs() []string {
	return []string{"-u", "-c", "import sys\nfor line in sys.stdin:\n    sys.stdout.write(line)\n    sys.stdout.flush()"}
}

// TestHostEnvAppendsToOSEnviron verifies that HostConfig.Env is properly
// appended to os.Environ() instead of replacing it. The subprocess should
// have access to both the parent's environment (e.g. PATH) and the config-provided vars.
func TestHostEnvAppendsToOSEnviron(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args: []string{"-u", "-c", `
import os, sys, json
out = {
    "has_path": bool(os.environ.get("PATH")),
    "custom": os.environ.get("CUSTOM_VAR", ""),
}
print(json.dumps(out))
sys.stdout.flush()
`},
		Env: map[string]string{"CUSTOM_VAR": "hello-phase-2"},
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	got, err := h.readStdoutTest(2 * time.Second)
	if err != nil {
		t.Fatalf("readStdoutTest: %v", err)
	}

	var result struct {
		HasPath bool   `json:"has_path"`
		Custom  string `json:"custom"`
	}
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.HasPath {
		t.Error("subprocess did not inherit PATH from parent environment")
	}
	if result.Custom != "hello-phase-2" {
		t.Errorf("CUSTOM_VAR: got %q, want %q", result.Custom, "hello-phase-2")
	}
}

// TestHostHTTPIDMultiplexing verifies that two concurrent HTTP clients each
// receive the response matching their original request id, even when the
// host rewrites ids internally to route them to one shared subprocess.
//
// The echo subprocess returns the request unchanged — so we can assert that
// the id we sent is the id we got back, per-client.
func TestHostHTTPIDMultiplexing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: echoSubprocCommand(),
		Args:    echoSubprocArgs(),
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// Two clients send requests with id=100 and id=200 concurrently.
	// Each must receive back its own id, not the other client's.
	sendAndAssert := func(t *testing.T, id int) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, id)
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("POST id=%d: %v", id, err)
			return
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		var msg map[string]any
		if err := json.Unmarshal(got, &msg); err != nil {
			t.Errorf("id=%d: bad JSON response: %v (body=%s)", id, err, got)
			return
		}
		if gotID, ok := msg["id"].(float64); !ok || int(gotID) != id {
			t.Errorf("id=%d: response id mismatch: got %v", id, msg["id"])
		}
	}

	var wg sync.WaitGroup
	for _, id := range []int{100, 200, 300} {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sendAndAssert(t, id)
		}(id)
	}
	wg.Wait()
}

// TestHostInitializeCached verifies that after the first client sends
// `initialize`, subsequent `initialize` requests return the cached response
// without being forwarded to the subprocess. This is the contract
// for sharing one subprocess across N MCP clients.
func TestHostInitializeCached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Scripted subprocess: responds to the first `initialize` with a
	// fixed result, then the test asserts the second request never reaches it
	// by checking the stdin counter from inside the script.
	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args: []string{"-u", "-c", `
import sys, json
seen = 0
for line in sys.stdin:
    msg = json.loads(line)
    seen += 1
    if msg.get("method") == "initialize":
        resp = {"jsonrpc":"2.0","id":msg["id"],"result":{"protocolVersion":"2025-03-26","capabilities":{},"seen":seen}}
        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()
`},
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`

	doInit := func() map[string]any {
		resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(initBody))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		return msg
	}

	r1 := doInit()
	r2 := doInit()

	// Both should have the same `result.seen` value (1), because the second
	// request was served from the cache, not forwarded to the subprocess.
	result1, _ := r1["result"].(map[string]any)
	result2, _ := r2["result"].(map[string]any)
	if result1["seen"] != 1.0 || result2["seen"] != 1.0 {
		t.Errorf("initialize cache not used: r1.seen=%v r2.seen=%v (both should be 1)", result1["seen"], result2["seen"])
	}
}

// TestHostStopUnblocksPendingHandlers verifies that calling Stop() while
// a handler is waiting for a subprocess response unblocks the handler
// immediately with 503 instead of waiting the full 30s timeout.
func TestHostStopUnblocksPendingHandlers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Silent subprocess: reads stdin but never writes stdout.
	h, err := NewStdioHost(HostConfig{
		Command: "python",
		Args:    []string{"-u", "-c", "import sys\nfor _ in sys.stdin: pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// Start a request that will hang waiting for subprocess response.
	done := make(chan int, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/mcp", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"test"}`))
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()

	// Give the handler a moment to enter the select.
	time.Sleep(200 * time.Millisecond)

	// Stop should unblock the handler quickly.
	stopAt := time.Now()
	_ = h.Stop()
	unblocked := time.Since(stopAt)
	if unblocked > 2*time.Second {
		t.Errorf("Stop did not unblock handler quickly: took %v", unblocked)
	}

	select {
	case status := <-done:
		if status != http.StatusServiceUnavailable {
			t.Errorf("handler got status %d, want 503", status)
		}
	case <-time.After(3 * time.Second):
		t.Error("handler did not return within 3s after Stop")
	}
}

// TestHostStopUnblocksSSE verifies that calling Stop() while an SSE client
// is subscribed unblocks the handler quickly instead of hanging forever.
func TestHostStopUnblocksSSE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{Command: echoSubprocCommand(), Args: echoSubprocArgs()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	// Open an SSE subscription in background; it will block reading.
	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		// Drain body until connection closes.
		io.Copy(io.Discard, resp.Body)
	}()

	// Let the SSE handler enter the stream loop.
	time.Sleep(200 * time.Millisecond)

	// Stop should unblock the SSE handler quickly.
	stopAt := time.Now()
	_ = h.Stop()
	unblocked := time.Since(stopAt)
	if unblocked > 2*time.Second {
		t.Errorf("Stop did not unblock SSE handler quickly: took %v", unblocked)
	}

	select {
	case <-sseDone:
		// Handler exited — good.
	case <-time.After(3 * time.Second):
		t.Error("SSE handler did not return within 3s after Stop")
	}
}

// TestHostDELETETerminates verifies DELETE /mcp is accepted.
func TestHostDELETETerminates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := NewStdioHost(HostConfig{Command: echoSubprocCommand(), Args: echoSubprocArgs()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	req, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 200 or 204", resp.StatusCode)
	}
}
