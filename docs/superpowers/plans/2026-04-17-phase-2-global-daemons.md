# Phase 2 — Global MCP Daemons + Native Stdio-Host Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Consolidate 6 stateless/shared-state MCP servers (memory, sequential-thinking, wolfram, godbolt, paper-search-mcp, time) into shared daemons accessible via HTTP, and add context7 as a direct HTTP entry in Claude Code. Eliminate ~2 GB of duplicated RAM usage across ~60 stdio subprocess instances and fix the data-race risk in memory's JSONL store.

**Architecture:** Build a native Go stdio-host (`internal/daemon/host.go`) as the mirror-image of `internal/daemon/relay.go` — `relay.go` forwards stdio→HTTP (client side), `host.go` hosts an HTTP endpoint that multiplexes to a single stdio subprocess (daemon side). The host rewrites JSON-RPC request IDs to correlate concurrent HTTP clients against one shared subprocess, caches the subprocess's initialize response to answer all subsequent `initialize` requests without re-forwarding, and broadcasts stdio-server notifications to all active SSE streams. Each of the 6 new servers gets a YAML manifest in `servers/<name>/manifest.yaml` using `transport: stdio-bridge` which now routes through the native host instead of the external `supergateway` npm package. context7 is handled separately — it's already a remote HTTPS endpoint, so we only add the client binding for Claude Code.

**Tech Stack:** Go 1.22+ (stdlib `net/http`, `bufio`, `encoding/json`, `os/exec`, `sync`, `context`). Test harness uses `net/http/httptest` and an in-test stdio echo server built from a Go goroutine pair. No new external dependencies.

**Reference implementations:**

- [`mcp-proxy`](https://github.com/sparfenyuk/mcp-proxy) — Python reference for multi-client HTTP→stdio multiplexing, especially the initialize-cache pattern
- MCP Streamable HTTP spec: https://modelcontextprotocol.io/docs/concepts/transports
- Our own `internal/daemon/relay.go` — reverse direction (HTTP→stdio for a single client), useful as a shape reference for JSON-RPC parsing and SSE framing

**Spec reference:** `docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md` — §3.4.3 (stdio-bridge lane) and §3.6 (client adapter table)

**Prerequisites:**

- Phase 1 + post-Phase 1 complete: shared serena daemon on 9121/9122 working, relay.go tested, all 4 clients connect successfully
- `uvx` (Python), `node`, `npx` available on PATH (needed to launch the underlying stdio servers)
- Existing server binaries installed: wolfram at `C:/Users/USERNAME/.local/mcp-servers/wolframalpha-llm-mcp/build/index.js`, godbolt at `C:/Users/USERNAME/.local/mcp-servers/godbolt-mcp/...`
- User on Windows 11 (Linux/macOS scheduler stubs not in scope)

---

## Naming, ports, and invariants

**New port allocations** (all in the 9121–9139 global range):

| Server | Port |
|---|---|
| memory | 9123 |
| sequential-thinking | 9124 |
| wolfram | 9125 |
| godbolt | 9126 |
| paper-search-mcp | 9127 |
| time | 9128 |

**Scheduler task naming:** `mcp-local-hub-<server>-default` for each (e.g. `mcp-local-hub-memory-default`). Each new server has a single daemon named `default`.

**Env/secret resolution:**

- memory needs `MEMORY_FILE_PATH=c:/Users/USERNAME/OneDrive/Documents/env/Agents/memory.jsonl` — literal path, not a secret
- wolfram needs `WOLFRAM_LLM_APP_ID=EXAMPLE_APP_ID_123` — move to secret vault (`wolfram_app_id`)
- paper-search-mcp needs `PAPER_SEARCH_MCP_UNPAYWALL_EMAIL=user@example.com` — move to vault (`unpaywall_email`)
- Others have no env

**Protocol invariant:** one stdio subprocess per daemon, for the entire daemon lifetime. HTTP-side multi-client access is handled by ID-rewriting multiplexing. This is the correct behavior for memory (prevents data race) and safe for all stateless servers.

---

## File Structure

**Files to create:**

- `internal/daemon/host.go` — stdio-host core (~500 LOC): subprocess lifecycle, stdin writer, stdout reader loop, HTTP handler with ID rewriting, initialize cache, SSE broadcast, MCP-Session-Id handling
- `internal/daemon/host_test.go` — unit tests (~300 LOC): subprocess lifecycle, ID correlation, concurrent requests, initialize cache, SSE broadcast, graceful shutdown
- `servers/memory/manifest.yaml`
- `servers/sequential-thinking/manifest.yaml`
- `servers/wolfram/manifest.yaml`
- `servers/godbolt/manifest.yaml`
- `servers/paper-search-mcp/manifest.yaml`
- `servers/time/manifest.yaml`

**Files to modify:**

- `configs/ports.yaml` — register 6 new ports (9123–9128)
- `internal/cli/daemon.go` — fix manifest path to use `os.Executable()` (currently `filepath.Join("servers", ...)` is relative to CWD); route `stdio-bridge` transport through new `host.Run()` instead of `BuildBridgeSpec`/supergateway
- `internal/daemon/bridge.go` — keep only for reference, mark as deprecated in comment (or remove if unused after Task 5); decision made in Task 5 based on whether anything still references it
- `internal/clients/claude_code.go` — no code change expected; AddEntry already replaces existing `mcpServers[name]` entry so replacing stdio→http "just works". Verify with test in Task 13.
- `README.md`, `INSTALL.md`, `docs/phase-1-verification.md` — Phase 2 status update, new server docs

---

## Task breakdown

Total tasks: 14. Each task is independently committable and maps to one well-defined change.

---

### Task 1: Port allocation + scope documentation

**Files:**

- Modify: `configs/ports.yaml`
- Create: `docs/phase-2-scope.md` (short reference, not a full verification doc)

- [ ] **Step 1: Add new global ports to `configs/ports.yaml`**

Edit the file to add:

```yaml
global:
  - server: serena
    daemon: claude
    port: 9121
  - server: serena
    daemon: codex
    port: 9122
  - server: memory
    daemon: default
    port: 9123
  - server: sequential-thinking
    daemon: default
    port: 9124
  - server: wolfram
    daemon: default
    port: 9125
  - server: godbolt
    daemon: default
    port: 9126
  - server: paper-search-mcp
    daemon: default
    port: 9127
  - server: time
    daemon: default
    port: 9128
workspace_scoped: []
```

- [ ] **Step 2: Create `docs/phase-2-scope.md`**

Write this content:

```markdown
# Phase 2 scope

Adds 6 global MCP daemons + 1 external HTTP client binding (context7).

## Servers consolidated to shared daemons

| Server | Port | Runtime | Env needed |
|---|---:|---|---|
| memory | 9123 | node (npx) | MEMORY_FILE_PATH |
| sequential-thinking | 9124 | node (npx) | — |
| wolfram | 9125 | node | secret:wolfram_app_id → WOLFRAM_LLM_APP_ID |
| godbolt | 9126 | python (venv) | — |
| paper-search-mcp | 9127 | python (uvx) | secret:unpaywall_email → PAPER_SEARCH_MCP_UNPAYWALL_EMAIL |
| time | 9128 | node (npx) | — |

## Direct HTTP (no daemon)

| Server | URL | Reason |
|---|---|---|
| context7 | https://mcp.context7.com/mcp | already remote HTTPS; added to Claude Code only |

## New core component

`internal/daemon/host.go` — native Go stdio-host that replaces supergateway.
Mirror of `internal/daemon/relay.go`: relay = stdio→HTTP (client side),
host = HTTP→stdio (daemon side).
```

- [ ] **Step 3: Commit**

```bash
git add configs/ports.yaml docs/phase-2-scope.md
git commit -m "docs(phase-2): reserve ports 9123-9128 + scope doc"
```

---

### Task 2: stdio-host — subprocess lifecycle (start/stop, stdout line reader)

**Files:**

- Create: `internal/daemon/host.go`
- Create: `internal/daemon/host_test.go`

- [ ] **Step 1: Write the failing test — subprocess starts and writes are echoed to stdout**

Create `internal/daemon/host_test.go`:

```go
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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
```

Also in the same file, add the echo subprocess helper (platform-agnostic: uses `go run` of an inline Go program is heavy — simplest is a Python one-liner or platform-switched shell):

```go
// echoSubprocCommand returns a command that reads lines from stdin
// and writes each line back to stdout verbatim. Used only in tests.
func echoSubprocCommand() string {
	return "python"
}

func echoSubprocArgs() []string {
	return []string{"-u", "-c", "import sys\nfor line in sys.stdin:\n    sys.stdout.write(line)\n    sys.stdout.flush()"}
}
```

- [ ] **Step 2: Run test to verify it fails (no implementation yet)**

```bash
cd <repo> && go test ./internal/daemon/ -run TestHostSubprocessLifecycle -v
```

Expected: FAIL with `undefined: NewStdioHost`

- [ ] **Step 3: Write minimal `host.go` implementation**

Create `internal/daemon/host.go`:

```go
package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// HostConfig describes one stdio-host instance.
type HostConfig struct {
	Command    string            // subprocess executable
	Args       []string          // subprocess args
	Env        map[string]string // appended to os.Environ() for the subprocess
	WorkingDir string            // subprocess cwd; empty means inherit
}

// StdioHost hosts a long-lived stdio subprocess and (in later tasks) exposes
// an HTTP endpoint that multiplexes concurrent MCP clients onto it.
type StdioHost struct {
	cfg HostConfig

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutScan *bufio.Scanner

	// Test-only unbuffered channel for readStdoutTest.
	testStdout chan []byte

	mu      sync.Mutex
	started bool
	stopped bool
}

func NewStdioHost(cfg HostConfig) (*StdioHost, error) {
	if cfg.Command == "" {
		return nil, errors.New("HostConfig.Command is required")
	}
	return &StdioHost{
		cfg:        cfg,
		testStdout: make(chan []byte, 16),
	}, nil
}

// Start spawns the subprocess and begins the stdout reader goroutine.
// Returns an error if the subprocess fails to start.
func (h *StdioHost) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("already started")
	}

	cmd := exec.CommandContext(ctx, h.cfg.Command, h.cfg.Args...)
	cmd.Dir = h.cfg.WorkingDir
	if len(h.cfg.Env) > 0 {
		env := append([]string{}, cmd.Env...)
		for k, v := range h.cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Stderr is NOT part of the JSON-RPC protocol channel. Forward it to
	// os.Stderr (and thus the scheduler log file via Launch() tee in Task 5).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start subprocess: %w", err)
	}
	// Forward stderr line-by-line to os.Stderr for diagnostic visibility.
	go func() {
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			fmt.Fprintf(os.Stderr, "[subproc stderr] %s\n", s.Bytes())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1 MB lines

	h.cmd = cmd
	h.stdin = stdin
	h.stdoutScan = scanner
	h.started = true

	// Reader goroutine: pipes every stdout line to testStdout (and, in later
	// tasks, to the ID-routing map for HTTP response delivery).
	go h.readStdoutLoop()

	return nil
}

// Stop terminates the subprocess and closes all pipes.
func (h *StdioHost) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started || h.stopped {
		return nil
	}
	h.stopped = true
	_ = h.stdin.Close()
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_, _ = h.cmd.Process.Wait()
	}
	return nil
}

// writeStdin sends a line (terminated with '\n') to the subprocess stdin.
// Internal; exposed for the test helper only in this initial task.
func (h *StdioHost) writeStdin(line []byte) error {
	if _, err := h.stdin.Write(line); err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		if _, err := h.stdin.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

// readStdoutLoop is the subprocess stdout reader. For now it just forwards
// each raw line to testStdout. Task 3 wires it to the ID-routing map.
func (h *StdioHost) readStdoutLoop() {
	for h.stdoutScan.Scan() {
		line := append([]byte(nil), h.stdoutScan.Bytes()...)
		select {
		case h.testStdout <- line:
		default:
			// Drop if nobody is reading (prevents goroutine leak in tests).
		}
	}
}

// readStdoutTest exposes the raw stdout stream for unit tests only.
func (h *StdioHost) readStdoutTest(timeout time.Duration) ([]byte, error) {
	select {
	case line := <-h.testStdout:
		return line, nil
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for stdout line")
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd <repo> && go test ./internal/daemon/ -run TestHostSubprocessLifecycle -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/host.go internal/daemon/host_test.go
git commit -m "feat(daemon): stdio-host subprocess lifecycle + stdout reader"
```

---

### Task 3: stdio-host — HTTP handler with JSON-RPC ID rewriting

**Files:**

- Modify: `internal/daemon/host.go` (add HTTP handler, pending-request map, ID rewriter)
- Modify: `internal/daemon/host_test.go` (add concurrent request correlation test)

- [ ] **Step 1: Write the failing test — concurrent HTTP clients get correct responses**

Append to `internal/daemon/host_test.go`:

```go
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
```

Also add imports: `"fmt"`, `"io"`, `"net/http"`, `"net/http/httptest"`, `"strings"`, `"sync"`.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd <repo> && go test ./internal/daemon/ -run TestHostHTTPIDMultiplexing -v
```

Expected: FAIL with `h.HTTPHandler undefined`

- [ ] **Step 3: Implement HTTP handler + ID rewriting in `host.go`**

Add to `host.go`:

```go
// Add to imports: "encoding/json", "net/http", "strconv", "sync/atomic"

// Add pending-request tracking to StdioHost struct:
//   nextInternalID atomic.Int64
//   pendingMu      sync.Mutex
//   pending        map[int64]chan json.RawMessage
//
// Initialize pending in NewStdioHost: pending: make(map[int64]chan json.RawMessage)

// HTTPHandler returns the http.Handler that POSTs JSON-RPC to the subprocess.
// For now only POST /mcp is implemented; GET (SSE) and DELETE come in Task 4.
func (h *StdioHost) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handlePOST(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (h *StdioHost) handlePOST(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Notifications have no id; we forward-and-forget.
	origIDRaw, hasID := msg["id"]
	if !hasID {
		if err := h.writeStdin(body); err != nil {
			http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Rewrite id to an internal counter to avoid collisions across HTTP clients.
	internalID := h.nextInternalID.Add(1)
	msg["id"] = json.RawMessage(strconv.FormatInt(internalID, 10))
	rewritten, _ := json.Marshal(msg)

	respCh := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[internalID] = respCh
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, internalID)
		h.pendingMu.Unlock()
	}()

	if err := h.writeStdin(rewritten); err != nil {
		http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
		return
	}

	select {
	case respBody := <-respCh:
		// Restore the original id before returning to the HTTP client.
		var respMsg map[string]json.RawMessage
		if err := json.Unmarshal(respBody, &respMsg); err != nil {
			http.Error(w, "subprocess returned invalid JSON", http.StatusBadGateway)
			return
		}
		respMsg["id"] = origIDRaw
		out, _ := json.Marshal(respMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	case <-r.Context().Done():
		http.Error(w, "client canceled", http.StatusRequestTimeout)
	case <-time.After(30 * time.Second):
		http.Error(w, "subprocess response timeout", http.StatusGatewayTimeout)
	}
}
```

Update `readStdoutLoop` to dispatch by ID:

```go
func (h *StdioHost) readStdoutLoop() {
	for h.stdoutScan.Scan() {
		line := append([]byte(nil), h.stdoutScan.Bytes()...)
		// Try to parse id and route to pending request.
		var peek struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &peek); err == nil && len(peek.ID) > 0 {
			var id int64
			if err := json.Unmarshal(peek.ID, &id); err == nil {
				h.pendingMu.Lock()
				ch, ok := h.pending[id]
				h.pendingMu.Unlock()
				if ok {
					select {
					case ch <- line:
					default:
					}
					continue
				}
			}
		}
		// Unrouted line (notification or unknown id) — send to testStdout as fallback.
		select {
		case h.testStdout <- line:
		default:
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd <repo> && go test ./internal/daemon/ -run TestHostHTTPIDMultiplexing -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/host.go internal/daemon/host_test.go
git commit -m "feat(daemon): stdio-host HTTP handler with JSON-RPC id multiplexing"
```

---

### Task 4: stdio-host — initialize-cache + SSE + DELETE lifecycle

**Files:**

- Modify: `internal/daemon/host.go`
- Modify: `internal/daemon/host_test.go`

- [ ] **Step 1: Write failing tests for initialize-cache + SSE + DELETE**

Append to `host_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd <repo> && go test ./internal/daemon/ -run "TestHostInitializeCached|TestHostDELETETerminates" -v
```

Expected: both FAIL

- [ ] **Step 3: Add initialize-cache + DELETE to `host.go`**

In `StdioHost` struct, add:

```go
	initMu       sync.Mutex
	initCached   json.RawMessage // cached initialize response body (with `id` still rewritten)
	initOrigBody []byte          // original request body of the first initialize
```

Modify `handlePOST` to add initialize-cache short-circuit BEFORE the id-rewrite block:

```go
	// If this is an `initialize` request and we already have a cached
	// response from an earlier client, return it without forwarding.
	if methodRaw, ok := msg["method"]; ok {
		var method string
		_ = json.Unmarshal(methodRaw, &method)
		if method == "initialize" {
			h.initMu.Lock()
			cached := h.initCached
			h.initMu.Unlock()
			if cached != nil {
				// Rewrite cached response with this client's id.
				var respMsg map[string]json.RawMessage
				_ = json.Unmarshal(cached, &respMsg)
				respMsg["id"] = origIDRaw
				out, _ := json.Marshal(respMsg)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(out)
				return
			}
		}
	}
```

After the response is received from subprocess (before returning), cache if it was an `initialize`:

```go
	case respBody := <-respCh:
		// If this was initialize, cache the response for future clients.
		if methodRaw, ok := msg["method"]; ok {
			var method string
			_ = json.Unmarshal(methodRaw, &method)
			if method == "initialize" {
				h.initMu.Lock()
				if h.initCached == nil {
					// Cache the raw response — id will be rewritten per client.
					cached := append(json.RawMessage(nil), respBody...)
					h.initCached = cached
				}
				h.initMu.Unlock()
			}
		}
		// ... existing id-restore + response-write code
```

Wait, note the above has bugs because `origIDRaw` is captured before the initialize-cache check. Let me restructure — move `origIDRaw` capture earlier and use it in both paths.

**Correct structure:** after parsing `msg`, immediately capture `origIDRaw` (default to `null` for notifications), then check initialize-cache, then do the id-rewrite + subprocess forward. Full `handlePOST` after this task:

```go
func (h *StdioHost) handlePOST(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	origIDRaw, hasID := msg["id"]

	// Initialize-cache short-circuit.
	if methodRaw, ok := msg["method"]; ok && hasID {
		var method string
		_ = json.Unmarshal(methodRaw, &method)
		if method == "initialize" {
			h.initMu.Lock()
			cached := h.initCached
			h.initMu.Unlock()
			if cached != nil {
				var respMsg map[string]json.RawMessage
				_ = json.Unmarshal(cached, &respMsg)
				respMsg["id"] = origIDRaw
				out, _ := json.Marshal(respMsg)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(out)
				return
			}
		}
	}

	// Notifications: forward-and-forget.
	if !hasID {
		if err := h.writeStdin(body); err != nil {
			http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Rewrite id to avoid collisions across HTTP clients.
	internalID := h.nextInternalID.Add(1)
	msg["id"] = json.RawMessage(strconv.FormatInt(internalID, 10))
	rewritten, _ := json.Marshal(msg)

	respCh := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[internalID] = respCh
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, internalID)
		h.pendingMu.Unlock()
	}()

	if err := h.writeStdin(rewritten); err != nil {
		http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
		return
	}

	select {
	case respBody := <-respCh:
		// Cache initialize responses.
		if methodRaw, ok := msg["method"]; ok {
			var method string
			_ = json.Unmarshal(methodRaw, &method)
			if method == "initialize" {
				h.initMu.Lock()
				if h.initCached == nil {
					h.initCached = append(json.RawMessage(nil), respBody...)
				}
				h.initMu.Unlock()
			}
		}
		var respMsg map[string]json.RawMessage
		if err := json.Unmarshal(respBody, &respMsg); err != nil {
			http.Error(w, "subprocess returned invalid JSON", http.StatusBadGateway)
			return
		}
		respMsg["id"] = origIDRaw
		out, _ := json.Marshal(respMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	case <-r.Context().Done():
		http.Error(w, "client canceled", http.StatusRequestTimeout)
	case <-time.After(30 * time.Second):
		http.Error(w, "subprocess response timeout", http.StatusGatewayTimeout)
	}
}
```

Add DELETE support to `HTTPHandler`:

```go
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handlePOST(w, r)
		case http.MethodDelete:
			// Session termination: subprocess stays alive (shared across clients),
			// but we acknowledge the client's request. Nothing to clean up on our side
			// since pending requests are per-request scoped.
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			h.handleSSE(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
```

Add a basic `handleSSE` that accepts the connection but doesn't broadcast yet (broadcast wired in the same task if the test needs it — otherwise make this a stub that only opens a stream):

```go
// handleSSE registers a new SSE subscriber and keeps the connection open
// until the client disconnects. Notifications from the subprocess are
// broadcast to all active SSE subscribers.
func (h *StdioHost) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch := make(chan []byte, 32)
	h.sseMu.Lock()
	h.sseClients = append(h.sseClients, ch)
	h.sseMu.Unlock()
	defer func() {
		h.sseMu.Lock()
		for i, c := range h.sseClients {
			if c == ch {
				h.sseClients = append(h.sseClients[:i], h.sseClients[i+1:]...)
				break
			}
		}
		h.sseMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}
```

Add `sseMu sync.Mutex` and `sseClients []chan []byte` fields to `StdioHost`. Extend `readStdoutLoop` to broadcast unrouted lines (notifications) to SSE clients instead of just dropping them:

```go
		// Unrouted line = notification → broadcast to SSE subscribers.
		h.sseMu.Lock()
		for _, c := range h.sseClients {
			select {
			case c <- line:
			default:
			}
		}
		h.sseMu.Unlock()
		// Also keep the testStdout for tests that watch unrouted lines.
		select {
		case h.testStdout <- line:
		default:
		}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd <repo> && go test ./internal/daemon/ -v
```

Expected: all tests PASS (4 so far: lifecycle, multiplexing, initialize-cache, DELETE)

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/host.go internal/daemon/host_test.go
git commit -m "feat(daemon): stdio-host initialize cache + SSE + DELETE handlers"
```

---

### Task 5: Wire stdio-host into `daemon.go` + fix manifest-path bug

**Files:**

- Modify: `internal/cli/daemon.go`
- Modify: `internal/daemon/bridge.go` (add deprecation comment; keep code for reference)

- [ ] **Step 1: Fix manifest-path resolution in `daemon.go`**

Current code (line 24): `manifestPath := filepath.Join("servers", server, "manifest.yaml")` — this is relative to CWD, fragile if scheduler task WorkingDir drifts.

Change to mirror `relay.go`'s approach:

```go
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve executable: %w", err)
			}
			manifestPath := filepath.Join(filepath.Dir(exe), "servers", server, "manifest.yaml")
			f, err := os.Open(manifestPath)
			if err != nil {
				return fmt.Errorf("open manifest %s: %w", manifestPath, err)
			}
```

- [ ] **Step 2: Replace `stdio-bridge` supergateway path with native host**

Find this block in `daemon.go`:

```go
		} else if m.Transport == config.TransportStdioBridge {
			ls := daemon.BuildBridgeSpec(m.Command, childArgs, spec.Port, env, logPath)
			code, err := daemon.Launch(ls)
			if err != nil {
				return err
			}
			os.Exit(code)
		}
```

Replace with:

```go
		} else if m.Transport == config.TransportStdioBridge {
			h, err := daemon.NewStdioHost(daemon.HostConfig{
				Command: m.Command,
				Args:    childArgs,
				Env:     env,
			})
			if err != nil {
				return fmt.Errorf("NewStdioHost: %w", err)
			}
			ctx := cmd.Context()
			if err := h.Start(ctx); err != nil {
				return fmt.Errorf("host.Start: %w", err)
			}
			srv := &http.Server{
				Addr:    fmt.Sprintf("127.0.0.1:%d", spec.Port),
				Handler: h.HTTPHandler(),
			}
			// Tee subprocess logs to LogPath via a wrapper (left to host logging).
			// For now log to stderr; scheduler captures via WorkingDir+log setup.
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			select {
			case err := <-errCh:
				_ = h.Stop()
				return fmt.Errorf("http server: %w", err)
			case <-ctx.Done():
				_ = srv.Shutdown(context.Background())
				_ = h.Stop()
				return nil
			}
		}
```

Add imports: `"context"`, `"net/http"`.

- [ ] **Step 3: Mark `bridge.go` as deprecated**

At the top of `internal/daemon/bridge.go`, replace the file's leading comment (before `package daemon`) with:

```go
// Package daemon — bridge.go is DEPRECATED as of Phase 2.
//
// Original role: wrapped a stdio MCP server in `npx supergateway` to expose it
// over HTTP. Replaced by internal/daemon/host.go, a native Go stdio-host that
// handles the HTTP→stdio proxy without requiring node/npm. bridge.go is kept
// for reference only; no production code path invokes it.
```

- [ ] **Step 4: Run all tests to verify nothing regressed**

```bash
cd <repo> && go test ./... -v
```

Expected: all PASS, including new host tests, existing relay tests, and daemon-cli tests.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/daemon.go internal/daemon/bridge.go
git commit -m "feat(daemon): route stdio-bridge through native Go host, fix manifest path"
```

---

### Task 6: memory manifest + install + live test

**Files:**

- Create: `servers/memory/manifest.yaml`

- [ ] **Step 1: Create `servers/memory/manifest.yaml`**

```yaml
name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-memory"
env:
  MEMORY_FILE_PATH: "c:/Users/USERNAME/OneDrive/Documents/env/Agents/memory.jsonl"

daemons:
  - name: default
    port: 9123

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 2: Rebuild mcp.exe**

```bash
cd <repo> && ./build.sh
```

Expected: successful build, `mcp.exe` created.

- [ ] **Step 3: Dry-run install to inspect plan**

```bash
cd <repo> && ./mcp.exe install --server memory --dry-run
```

Expected output includes 1 scheduler task `mcp-local-hub-memory-default` + 4 client updates.

- [ ] **Step 4: Kill any existing stdio-based memory subprocesses**

```bash
taskkill //F //IM node.exe 2>/dev/null | grep -c "server-memory" || true
```

- [ ] **Step 5: Apply install**

```bash
cd <repo> && ./mcp.exe install --server memory
```

Expected: all 4 client bindings written, daemon on 9123 started.

- [ ] **Step 6: Verify via curl — initialize handshake**

```bash
curl -s -X POST http://127.0.0.1:9123/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: JSON response with `"serverInfo":{"name":"memory-server"...}` or similar (exact shape depends on the server).

- [ ] **Step 7: Verify via Claude Code inside this session**

```bash
claude mcp get memory
```

Expected: `Status: ✓ Connected, Type: http, URL: http://localhost:9123/mcp`

- [ ] **Step 8: Commit manifest**

```bash
git add servers/memory/manifest.yaml
git commit -m "feat(servers): add memory global daemon (shared JSONL store, port 9123)"
```

---

### Task 7: sequential-thinking manifest + install + live test

**Files:**

- Create: `servers/sequential-thinking/manifest.yaml`

- [ ] **Step 1: Create `servers/sequential-thinking/manifest.yaml`**

```yaml
name: sequential-thinking
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-sequential-thinking"

daemons:
  - name: default
    port: 9124

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 2: Install and verify**

```bash
cd <repo> && ./mcp.exe install --server sequential-thinking
curl -s -X POST http://127.0.0.1:9124/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: `initialize` response with server capabilities.

- [ ] **Step 3: Commit**

```bash
git add servers/sequential-thinking/manifest.yaml
git commit -m "feat(servers): add sequential-thinking global daemon (port 9124)"
```

---

### Task 8: wolfram manifest + secret migration + install + live test

**Files:**

- Create: `servers/wolfram/manifest.yaml`

- [ ] **Step 1: Move wolfram APP ID to the encrypted vault**

```bash
cd <repo> && ./mcp.exe secrets set wolfram_app_id --value EXAMPLE_APP_ID_123
./mcp.exe secrets list
```

Expected: `wolfram_app_id` appears in the list.

- [ ] **Step 2: Create `servers/wolfram/manifest.yaml`**

```yaml
name: wolfram
kind: global
transport: stdio-bridge
command: node
base_args:
  - "C:/Users/USERNAME/.local/mcp-servers/wolframalpha-llm-mcp/build/index.js"
env:
  WOLFRAM_LLM_APP_ID: "secret:wolfram_app_id"

daemons:
  - name: default
    port: 9125

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 3: Install and verify**

```bash
cd <repo> && ./mcp.exe install --server wolfram
curl -s -X POST http://127.0.0.1:9125/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: `initialize` response with wolfram server info.

- [ ] **Step 4: Commit**

```bash
git add servers/wolfram/manifest.yaml
git commit -m "feat(servers): add wolfram global daemon (port 9125, APP_ID from vault)"
```

---

### Task 9: godbolt manifest + install + live test

**Files:**

- Create: `servers/godbolt/manifest.yaml`

- [ ] **Step 1: Create `servers/godbolt/manifest.yaml`**

Godbolt ships as a Python script in its own venv. The base command is the venv's python directly — no env vars needed.

```yaml
name: godbolt
kind: global
transport: stdio-bridge
command: C:/Users/USERNAME/.local/mcp-servers/godbolt-mcp/venv/Scripts/python.exe
base_args:
  - "C:/Users/USERNAME/.local/mcp-servers/godbolt-mcp/godbolt_mcp.py"

daemons:
  - name: default
    port: 9126

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 2: Install and verify**

```bash
cd <repo> && ./mcp.exe install --server godbolt
curl -s -X POST http://127.0.0.1:9126/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: initialize response.

- [ ] **Step 3: Commit**

```bash
git add servers/godbolt/manifest.yaml
git commit -m "feat(servers): add godbolt global daemon (port 9126)"
```

---

### Task 10: paper-search-mcp manifest + secret migration + install + live test

**Files:**

- Create: `servers/paper-search-mcp/manifest.yaml`

- [ ] **Step 1: Move unpaywall email to the vault**

```bash
cd <repo> && ./mcp.exe secrets set unpaywall_email --value user@example.com
```

- [ ] **Step 2: Create `servers/paper-search-mcp/manifest.yaml`**

```yaml
name: paper-search-mcp
kind: global
transport: stdio-bridge
command: uvx
base_args:
  - "--from"
  - "paper-search-mcp"
  - "python"
  - "-m"
  - "paper_search_mcp.server"
env:
  PAPER_SEARCH_MCP_UNPAYWALL_EMAIL: "secret:unpaywall_email"

daemons:
  - name: default
    port: 9127

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 3: Install and verify**

```bash
cd <repo> && ./mcp.exe install --server paper-search-mcp
curl -s -X POST http://127.0.0.1:9127/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: initialize response.

- [ ] **Step 4: Commit**

```bash
git add servers/paper-search-mcp/manifest.yaml
git commit -m "feat(servers): add paper-search-mcp global daemon (port 9127)"
```

---

### Task 11: time manifest + install + live test

**Files:**

- Create: `servers/time/manifest.yaml`

- [ ] **Step 1: Create `servers/time/manifest.yaml`**

```yaml
name: time
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@mcpcentral/mcp-time"

daemons:
  - name: default
    port: 9128

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
  - client: antigravity
    daemon: default
    url_path: /mcp

weekly_refresh: false
```

- [ ] **Step 2: Install and verify**

```bash
cd <repo> && ./mcp.exe install --server time
curl -s -X POST http://127.0.0.1:9128/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
```

Expected: initialize response.

- [ ] **Step 3: Commit**

```bash
git add servers/time/manifest.yaml
git commit -m "feat(servers): add time global daemon (port 9128)"
```

---

### Task 12: context7 direct HTTP binding for Claude Code

**Rationale:** context7 is already a remote HTTPS service (`https://mcp.context7.com/mcp`). Codex CLI, Gemini CLI, and Antigravity already have it configured as direct HTTP. Only Claude Code lacks the entry. No daemon is needed — we just write an HTTP entry to `~/.claude.json`.

There is no manifest for this because it's not a daemon. Instead, this is a one-shot change via a tiny helper command or manual edit. We pick the minimal approach: manual JSON edit in this session.

**Files:**

- Modify: `~/.claude.json` (user config, not repo)

- [ ] **Step 1: Back up Claude Code config**

```bash
cp ~/.claude.json ~/.claude.json.bak-phase2-context7
```

- [ ] **Step 2: Add context7 entry via `claude mcp add`**

Claude Code supports adding HTTP MCP via its CLI. Use this instead of hand-editing JSON to avoid a malformed file:

```bash
claude mcp add --transport http context7 https://mcp.context7.com/mcp
```

If `claude mcp add` does not support the transport flag in this version, fall back to manual JSON edit: add this to `~/.claude.json` under `"mcpServers"`:

```json
    "context7": {
      "type": "http",
      "url": "https://mcp.context7.com/mcp"
    }
```

- [ ] **Step 3: Verify**

```bash
claude mcp get context7
```

Expected: `Status: ✓ Connected, Type: http, URL: https://mcp.context7.com/mcp`

- [ ] **Step 4: No commit** — this is a user-config change, not a repo change.

---

### Task 13: End-to-end integration + regression check across all clients

**Files:** none modified; this is verification only.

- [ ] **Step 1: Check all daemons are listening**

```bash
./mcp.exe status
```

Expected: 9 tasks Running — serena×2, memory, sequential-thinking, wolfram, godbolt, paper-search-mcp, time, weekly-refresh.

```bash
netstat -ano | grep -E ":(912[1-8])" | grep LISTENING
```

Expected: 8 LISTENING lines (9121-9128).

- [ ] **Step 2: Verify each daemon responds to initialize**

```bash
for port in 9121 9122 9123 9124 9125 9126 9127 9128; do
  echo "--- port $port ---"
  curl -s -X POST http://127.0.0.1:$port/mcp \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}' \
    | python -c 'import sys,json; d=json.load(sys.stdin); print(d.get("result",{}).get("serverInfo",{}).get("name","?"))'
done
```

Expected: each port prints the server name (Serena, memory-server, etc.).

- [ ] **Step 3: Claude Code — list all connected MCP**

```bash
claude mcp list
```

Expected: `serena`, `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time`, `context7` — all with ✓ Connected.

- [ ] **Step 4: Measure RAM delta**

```bash
python -c "
import subprocess
result = subprocess.run(['wmic','process','get','Name,CommandLine,WorkingSetSize','/format:csv'],capture_output=True,text=True,timeout=10)
targets = ['server-memory','sequential-thinking','wolfram','godbolt-mcp','paper_search_mcp','mcp-time']
total = 0
for line in result.stdout.strip().split('\n'):
    if any(t in line for t in targets):
        try: total += int(line.split(',')[-1])
        except: pass
print(f'Stdio subprocess RAM (should be low now): {total/(1024*1024):.0f} MB')
"
```

Expected: significantly lower than the pre-Phase-2 baseline (~2 GB) — stdio subprocesses are only the ones hosted by mcp.exe daemons.

- [ ] **Step 5: Kill legacy per-client stdio subprocesses so the comparison is clean**

Any `node.exe -y @modelcontextprotocol/server-memory`, `node.exe -y @modelcontextprotocol/server-sequential-thinking`, etc. that belonged to the old per-client spawns should be killed. Clients that are still running will respawn through the new HTTP path on their next request.

```bash
# Nothing here should kill our own mcp.exe daemons — target stdio subprocesses spawned by clients.
# Inspect first; kill by PID selectively.
wmic process where "name='node.exe'" get ProcessId,CommandLine | grep -iE "server-memory|server-sequential|mcp-time|context7|wolfram"
# Then: taskkill //PID <pid> //F   for each stdio one.
```

- [ ] **Step 6: Antigravity live test**

Restart Antigravity and confirm Cascade sees the new servers through the relay + daemons:

```powershell
Get-Process -Name Antigravity | Stop-Process -Force
Start-Sleep 3
Start-Process "$env:LOCALAPPDATA\Programs\Antigravity\Antigravity.exe"
```

Then ask Cascade: "list your MCP tools" and verify `memory`, `sequential-thinking`, `wolfram`, `godbolt`, `paper-search-mcp`, `time` all appear. Serena and context7 should continue to work.

Note: each new server in Antigravity's `mcp_config.json` is written as a relay-stdio entry (same pattern as serena) — Antigravity's adapter already handles this.

- [ ] **Step 7: No commit from this task** — integration-only.

---

### Task 14: Documentation update for Phase 2

**Files:**

- Modify: `README.md`
- Modify: `INSTALL.md`
- Modify: `docs/phase-1-verification.md` (append a `## Phase 2 verification` section)
- Create: `docs/phase-2-verification.md` (new doc for the live matrix)

- [ ] **Step 1: Update `README.md`**

In the architecture diagram / supported-clients section, replace the single-server (serena) mention with the new list. Replace the "Current status" line "Phase 1 + post-Phase 1 relay complete" with:

```
**Phase 2 complete** (2026-04-17). 7 MCP servers now run as shared daemons:
serena (×2 contexts), memory, sequential-thinking, wolfram, godbolt,
paper-search-mcp, time. Plus context7 as a direct HTTP entry across all
clients. Native Go stdio-host (`internal/daemon/host.go`) replaces the
`supergateway` npm dependency for stdio-to-HTTP bridging.
```

Update the "Key commands" table to note `stdio-bridge` now uses native Go host.

- [ ] **Step 2: Update `INSTALL.md`**

Add a "Per-server notes" subsection under the serena section, covering:

- memory: requires `MEMORY_FILE_PATH` — update the value in `servers/memory/manifest.yaml` if your JSONL path differs
- wolfram: requires `WOLFRAM_LLM_APP_ID` in the secret vault (`mcp secrets set wolfram_app_id`)
- paper-search-mcp: requires `UNPAYWALL_EMAIL` in the vault (`mcp secrets set unpaywall_email`)
- godbolt, sequential-thinking, time: no env needed

- [ ] **Step 3: Create `docs/phase-2-verification.md`**

```markdown
# Phase 2 Verification — 2026-04-17

Closes the Phase 2 plan `docs/superpowers/plans/2026-04-17-phase-2-global-daemons.md`.

## Servers

| Server | Port | Transport | Env | Clients |
|---|---:|---|---|---|
| serena (claude) | 9121 | native-http | — | Claude Code, Gemini CLI, Antigravity (relay) |
| serena (codex) | 9122 | native-http | — | Codex CLI |
| memory | 9123 | stdio-bridge (Go host) | MEMORY_FILE_PATH | all 4 |
| sequential-thinking | 9124 | stdio-bridge (Go host) | — | all 4 |
| wolfram | 9125 | stdio-bridge (Go host) | secret:wolfram_app_id | all 4 |
| godbolt | 9126 | stdio-bridge (Go host) | — | all 4 |
| paper-search-mcp | 9127 | stdio-bridge (Go host) | secret:unpaywall_email | all 4 |
| time | 9128 | stdio-bridge (Go host) | — | all 4 |

## RAM impact

Before Phase 2: ~76 MCP processes, ~3.5 GB.
After Phase 2: 1 subprocess per global server (6) + 2 serena (for a total of 8 "inner" MCP processes hosted by mcp.exe daemons). Each mcp.exe daemon adds ~15 MB of overhead. Net savings ≈ 2.0 GB depending on how many stale client stdio subprocesses are also cleaned up.

## Key architectural change

`internal/daemon/host.go` is a native Go HTTP→stdio host that replaces the `supergateway` npm package. It multiplexes N concurrent HTTP clients onto one subprocess stdin/stdout by rewriting JSON-RPC ids and caching the `initialize` response so only the first client triggers a real initialize against the subprocess.

## Live verification matrix

[fill in with actual results after Task 13]
```

- [ ] **Step 4: Append to `docs/phase-1-verification.md`**

```markdown
---

## Phase 2 (follow-on) — 2026-04-17

Phase 2 closed by `docs/phase-2-verification.md`. Summary:

- 6 new global daemons (memory, sequential-thinking, wolfram, godbolt, paper-search-mcp, time) on ports 9123–9128
- Native Go stdio-host (`internal/daemon/host.go`) replaces `supergateway`
- context7 added to Claude Code as direct HTTPS (no daemon)
```

- [ ] **Step 5: Commit docs**

```bash
git add README.md INSTALL.md docs/phase-2-verification.md docs/phase-1-verification.md
git commit -m "docs(phase-2): update README, INSTALL, verification for global daemons"
```

---

## Out of scope (Phase 3+)

- **Per-project (workspace-scoped) daemons** for mcp-language-server (clangd, fortran, gopls, pyright, rust-analyzer, etc.). These require `mcp register --workspace <path> --lang <lang>` lifecycle management — deferred to Phase 3.
- **Per-session servers** (gdb, lldb, playwright): out of scope because their state is session-bound.
- **GUI installer** with a table/checkboxes for what's routed through the hub — captured as a post-Phase-2 project memory item.
- **Linux/macOS scheduler** backends: Windows-only remains until Phase 4.
