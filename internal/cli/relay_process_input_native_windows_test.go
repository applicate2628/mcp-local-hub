//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mcp-local-hub/internal/daemon"
)

type relayProcessNativeTransport struct{}

func (relayProcessNativeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: make(http.Header), Body: http.NoBody}, nil
}

// The test process owns the inherited input, but uses the actual CLI owner.
// Its parent keeps the write end open through observed child exit. Both an
// immediate stop and an already-blocking read must settle without parent EOF.
func TestRelayInputNativeInheritedPipe(t *testing.T) {
	if os.Getenv("MCPHUB_RX03_NATIVE_PIPE") != "1" {
		t.Skip("opt-in owned native pipe qualification")
	}
	if mode := os.Getenv("MCPHUB_RX03_PIPE_CHILD"); mode != "" {
		if mode != "immediate" && mode != "blocked" {
			t.Fatal("invalid owned native test mode")
		}
		input := &relayProcessInput{file: os.Stdin}
		interrupt, err := processRelayInputInterrupt(input)
		if err != nil || interrupt == nil {
			t.Fatalf("inherited synchronous pipe did not qualify: %v", err)
		}
		entered := make(chan struct{})
		reader := relayProcessReadFunc(func(p []byte) (int, error) {
			close(entered)
			return input.Read(p)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		cancelDone := make(chan struct{})
		go func() {
			defer close(cancelDone)
			<-entered
			if mode == "blocked" {
				// Integration scheduling only; the pre-submission regression
				// has a separate channel-controlled native test.
				timer := time.NewTimer(150 * time.Millisecond)
				defer timer.Stop()
				<-timer.C
			}
			cancel()
		}()
		r := &daemon.HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: reader, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: &http.Client{Transport: relayProcessNativeTransport{}}, InterruptStdin: func() error { calls.Add(1); return interrupt() }}
		if err := r.Run(ctx); err != nil {
			t.Fatalf("inherited native read did not settle cleanly: %v", err)
		}
		<-cancelDone
		if calls.Load() != 1 {
			t.Fatalf("native interrupt count=%d", calls.Load())
		}
		if _, err := os.Stdin.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("owned native input still accepts reads: %v", err)
		}
		fmt.Fprintln(os.Stdout, "RX03_NATIVE_INPUT_SETTLED")
		return
	}
	for _, mode := range []string{"immediate", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRelayInputNativeInheritedPipe$", "-test.count=1", "-test.timeout=6s")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "MCPHUB_RX03_PIPE_CHILD=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "MCPHUB_RX03_PIPE_CHILD="+mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait() // no parent EOF before observed child settlement
			if err != nil {
				t.Fatalf("native child failed: %v ctx=%v stdout=%q stderr=%q", err, ctx.Err(), stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "RX03_NATIVE_INPUT_SETTLED") {
				t.Fatalf("missing native settlement marker: %q", stdout.String())
			}
			if ctx.Err() != nil {
				t.Fatal("native test reached watchdog")
			}
		})
	}
}
