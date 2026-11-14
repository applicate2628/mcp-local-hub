//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The parent leaves stdin open. Only the child process bootstrap may grant
// relay permission to stop its inherited read after the output stream fails.
func TestProcessRelayInputBrokenOutputSettles(t *testing.T) {
	if os.Getenv("MCPHUB_RX03_MAIN_NATIVE") != "1" {
		t.Skip("opt-in owned process-bootstrap test")
	}
	if url := os.Getenv("MCPHUB_RX03_MAIN_CHILD_URL"); url != "" {
		os.Args = []string{os.Args[0], "relay", "--url", url}
		main()
		t.Fatal("broken output unexpectedly returned successful main")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	getStarted, sendEvent := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("MCP-Session-Id", "owned-process-session")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case http.MethodGet:
			once.Do(func() { close(getStarted) })
			select {
			case <-sendEvent:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProcessRelayInputBrokenOutputSettles$", "-test.count=1", "-test.timeout=5s")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "MCPHUB_RX03_MAIN_CHILD_URL=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "MCPHUB_RX03_MAIN_CHILD_URL="+srv.URL)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, childOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer childOutput.Close()
	cmd.Stdout = childOutput
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cancel()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	_ = childOutput.Close() // parent does not keep the child's write end alive
	if _, err := io.WriteString(stdin, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":1`) {
		t.Fatalf("initialize failed: line=%q err=%v", line, err)
	}
	select {
	case <-getStarted:
	case <-ctx.Done():
		t.Fatal("SSE listener did not start")
	}
	// Integration settling window only. Ordering and late-read races are
	// separately tested deterministically in the daemon package.
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("parent deadline before fault injection")
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	close(sendEvent) // now the server's event hits a genuinely broken OS pipe
	err = cmd.Wait() // stdin writer is still open here: no parent-provided EOF
	if ctx.Err() != nil {
		t.Fatalf("relay failed to stop its input before watchdog: %v stderr=%q", ctx.Err(), stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("unexpected process outcome: %v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "relay stdout:") {
		t.Fatalf("output failure was not preserved by real main: %q", stderr.String())
	}
}

func TestProcessRelayInputDoesNotRequireStdinForVersion(t *testing.T) {
	if os.Getenv("MCPHUB_RX03_MAIN_NATIVE") != "1" {
		t.Skip("opt-in owned bootstrap test")
	}
	if os.Getenv("MCPHUB_RX03_VERSION_CHILD") == "1" {
		os.Stdin = nil // this child has no input capability to grant
		os.Args = []string{os.Args[0], "--version"}
		main()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProcessRelayInputDoesNotRequireStdinForVersion$", "-test.count=1", "-test.timeout=4s")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "MCPHUB_RX03_VERSION_CHILD=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "MCPHUB_RX03_VERSION_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "mcphub version ") {
		t.Fatalf("unrelated --version depends on stdin: %v, %q", err, output)
	}
}
