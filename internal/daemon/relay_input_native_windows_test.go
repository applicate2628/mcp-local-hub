//go:build windows

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This adapter belongs only to the test process which owns the native input.
// It is NOT selected by inspecting a borrowed Reader's concrete type.
type relayOwnedPipeTestInput struct {
	file     *os.File
	stopping atomic.Bool
}

func (r *relayOwnedPipeTestInput) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if r.stopping.Load() && errors.Is(err, os.ErrClosed) {
		err = context.Canceled
	}
	return n, err
}
func (r *relayOwnedPipeTestInput) interrupt() error {
	r.stopping.Store(true)
	return r.file.Close()
}

// Opt-in because this starts one hidden, owned test subprocess. The parent
// keeps its write end open until the child has exited: the child's read must
// end through its own authorized interruption, not a parent-supplied EOF.
func TestRelayInputNativeInheritedPipe(t *testing.T) {
	if os.Getenv("MCPHUB_RX03_NATIVE_PIPE") != "1" {
		t.Skip("opt-in owned native pipe qualification")
	}
	if os.Getenv("MCPHUB_RX03_PIPE_CHILD") == "1" {
		input := &relayOwnedPipeTestInput{file: os.Stdin}
		entered := make(chan struct{})
		reader := &relayOutputStartedReader{Reader: input, entered: entered}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		cancelDone := make(chan struct{})
		go func() {
			defer close(cancelDone)
			<-entered
			// Native integration deadline; the deterministic scheduling tests
			// separately cover cancellation before/after starting a read.
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			<-timer.C
			cancel()
		}()
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: reader, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error { calls.Add(1); return input.interrupt() }}
		if err := r.Run(ctx); err != nil {
			t.Fatalf("inherited native read did not settle cleanly: %v", err)
		}
		<-cancelDone
		if calls.Load() != 1 {
			t.Fatalf("native interrupt count=%d", calls.Load())
		}
		// Stat on Windows can return ERROR_INVALID_HANDLE rather than the Go
		// ErrClosed sentinel. Read checks the same closed FD state we own.
		if _, err := os.Stdin.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("owned native input still accepts reads: %v", err)
		}
		fmt.Fprintln(os.Stdout, "RX03_NATIVE_INPUT_SETTLED")
		return
	}
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
	cmd.Env = append(cmd.Env, "MCPHUB_RX03_PIPE_CHILD=1")
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
}
