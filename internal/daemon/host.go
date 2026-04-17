package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
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
	stdinMu sync.Mutex     // serializes writeStdin so concurrent callers cannot interleave on the wire
	wg      sync.WaitGroup // tracks reader goroutines so Stop() can drain them
	started bool
	stopped bool

	// HTTP-side multiplexing: each incoming JSON-RPC request id is rewritten
	// to a monotonic internal id; readStdoutLoop dispatches the response back
	// to the waiting handler via the matching channel in `pending`.
	nextInternalID atomic.Int64
	pendingMu      sync.Mutex
	pending        map[int64]chan json.RawMessage
}

func NewStdioHost(cfg HostConfig) (*StdioHost, error) {
	if cfg.Command == "" {
		return nil, errors.New("HostConfig.Command is required")
	}
	return &StdioHost{
		cfg:        cfg,
		testStdout: make(chan []byte, 16),
		pending:    make(map[int64]chan json.RawMessage),
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
		env := append([]string{}, os.Environ()...)
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
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
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
	h.wg.Add(1)
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
	h.wg.Wait()
	return nil
}

// writeStdin sends a line (terminated with '\n') to the subprocess stdin.
// Safe for concurrent callers: the buffer+newline are concatenated into a
// single slice and sent under stdinMu so two callers cannot interleave on
// the JSON-RPC wire to the subprocess.
func (h *StdioHost) writeStdin(line []byte) error {
	h.stdinMu.Lock()
	defer h.stdinMu.Unlock()
	buf := line
	if len(line) == 0 || line[len(line)-1] != '\n' {
		b := make([]byte, 0, len(line)+1)
		b = append(b, line...)
		b = append(b, '\n')
		buf = b
	}
	_, err := h.stdin.Write(buf)
	return err
}

// readStdoutLoop is the subprocess stdout reader. It peeks at each line's
// JSON-RPC id and dispatches it to the corresponding waiting HTTP handler
// via the `pending` map. Lines without a matching pending entry (e.g.
// notifications, server-initiated messages, or unrouted ids) fall through
// to testStdout so unit tests can still observe raw subprocess output.
func (h *StdioHost) readStdoutLoop() {
	defer h.wg.Done()
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

// readStdoutTest exposes the raw stdout stream for unit tests only.
func (h *StdioHost) readStdoutTest(timeout time.Duration) ([]byte, error) {
	select {
	case line := <-h.testStdout:
		return line, nil
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for stdout line")
	}
}
