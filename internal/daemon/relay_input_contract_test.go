package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func relayInputOnlyClient() *http.Client {
	return &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: make(http.Header), Body: http.NoBody}, nil
	})}
}

func TestRelayInputInterruptUnblocksAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error {
			calls.Add(1)
			return writer.CloseWithError(context.Canceled)
		}}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("authorized interrupt calls=%d, want one", calls.Load())
			_ = writer.CloseWithError(context.Canceled) // owned cleanup on regression
		}
		if err := <-done; err != nil {
			t.Fatalf("clean authorized cancellation failed: %v", err)
		}
	})
}

func TestRelayInputInterruptWaitsForCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered, readReleased, finishInterrupt := make(chan struct{}), make(chan struct{}), make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		interruptErr := errors.New("late input cleanup failure")
		var calls atomic.Int32
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error {
			calls.Add(1)
			_ = writer.CloseWithError(context.Canceled)
			close(readReleased)
			<-finishInterrupt
			return interruptErr
		}}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Error("input interruption never started")
			_ = writer.CloseWithError(context.Canceled)
			close(finishInterrupt)
			<-done
			return
		}
		<-readReleased
		synctest.Wait()
		var early error
		returnedEarly := false
		select {
		case early = <-done:
			returnedEarly = true
			t.Errorf("Run abandoned active interrupt callback: %v", early)
		default:
		}
		close(finishInterrupt)
		if !returnedEarly {
			early = <-done
		}
		if !errors.Is(early, interruptErr) {
			t.Fatalf("late interruption failure lost: %v", early)
		}
	})
}

func TestRelayInputInterruptFailureDoesNotAbandonRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		interruptErr, readErr := errors.New("native stop rejected"), errors.New("original read failed")
		var calls atomic.Int32
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error { calls.Add(1); return interruptErr }}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("interrupt not attempted once: %d", calls.Load())
		}
		var early error
		returnedEarly := false
		select {
		case early = <-done:
			returnedEarly = true
			t.Errorf("live borrowed read abandoned: %v", early)
		default:
		}
		_ = writer.CloseWithError(readErr) // explicit recovery by the actual owner
		if !returnedEarly {
			early = <-done
		}
		if !errors.Is(early, interruptErr) || !errors.Is(early, readErr) {
			t.Fatalf("independent stop/read outcomes not retained: %v", early)
		}
	})
}

func TestRelayInputInterruptDisarmedAtNormalEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error { calls.Add(1); return errors.New("must not interrupt settled input") }}
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatalf("late cancellation touched settled input: %d", calls.Load())
		}
	})
}

func TestRelayInputInterruptPreCanceledDoesNotTouchInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int32
		input := strings.NewReader("unused")
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error { calls.Add(1); return errors.New("not acquired") }}
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if calls.Load() != 0 || input.Len() != len("unused") {
			t.Fatalf("unstarted input touched: interrupts=%d unread=%d", calls.Load(), input.Len())
		}
	})
}

func TestRelayInputInterruptOnOutputFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		cause := errors.New("output failed with idle input")
		output := &relayFailingOutput{mode: "partial", failAt: 1, cause: cause, failed: make(chan struct{})}
		var calls atomic.Int32
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				<-entered
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\n"))}, nil
			}
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: output, Stderr: io.Discard, HTTPClient: client, InterruptStdin: func() error { calls.Add(1); return writer.CloseWithError(context.Canceled) }}
		r.setSessionID("fixture-session")
		done := make(chan error, 1)
		go func() { done <- r.Run(context.Background()) }()
		<-output.failed
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("output failure did not interrupt input: %d", calls.Load())
			_ = writer.CloseWithError(context.Canceled)
		}
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("output cause replaced by cancellation: %v", err)
		}
		if output.calls != 1 || output.closes != 0 {
			t.Fatalf("broken borrowed output used/closed: %+v", output)
		}
	})
}

type relayLateInput struct {
	entered, release chan struct{}
	payload          string
}

func (r *relayLateInput) Read(p []byte) (int, error) {
	if r.payload == "" {
		return 0, io.EOF
	}
	close(r.entered)
	<-r.release
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}
func TestRelayInputInterruptDoesNotDispatchLateRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input := &relayLateInput{entered: make(chan struct{}), release: make(chan struct{}), payload: "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n"}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var interrupts, posts atomic.Int32
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				posts.Add(1)
			}
			return &http.Response{StatusCode: 202, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client, InterruptStdin: func() error { interrupts.Add(1); close(input.release); return nil }}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-input.entered
		cancel()
		synctest.Wait()
		if interrupts.Load() != 1 {
			t.Errorf("missing interrupt: %d", interrupts.Load())
			close(input.release)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if posts.Load() != 0 {
			t.Fatalf("data observed after cancellation admitted %d HTTP calls", posts.Load())
		}
	})
}

func TestRelayInputInterruptPreservesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayOutputStartedReader{Reader: reader, entered: entered}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var calls atomic.Int32
		late := errors.New("late native close error")
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient(), InterruptStdin: func() error { calls.Add(1); _ = writer.CloseWithError(context.Canceled); return late }}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		err := <-done // synctest advances the virtual deadline when all work blocks
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, late) {
			t.Fatalf("deadline/cleanup outcomes collapsed: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("deadline interruption count=%d", calls.Load())
		}
	})
}

func TestRelayInputInterruptDisarmedWhileDrainingHTTP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		input := &relayErrorAfterLine{line: []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\"}\n"), err: io.EOF, observed: make(chan struct{})}
		requestStarted := make(chan struct{})
		var calls atomic.Int32
		client := &http.Client{Transport: relayOutputRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				close(requestStarted)
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return &http.Response{StatusCode: 405, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: client, InterruptStdin: func() error { calls.Add(1); return errors.New("settled input no longer belongs to this callback") }}
		r.setSessionID("fixture-session")
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-input.observed
		<-requestStarted
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("pending HTTP work abandoned: %v", err)
		default:
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 0 {
			t.Fatalf("network shutdown invoked expired input capability: %d", calls.Load())
		}
	})
}

// Expose Close as well as Read: method availability is not an ownership grant.
type relayBorrowedInputCloser struct {
	*relayOutputStartedReader
	closes atomic.Int32
	close  func() error
}

func (r *relayBorrowedInputCloser) Close() error { r.closes.Add(1); return r.close() }

func TestRelayInputBorrowedCloserIsNotImplicitCapability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		entered := make(chan struct{})
		input := &relayBorrowedInputCloser{relayOutputStartedReader: &relayOutputStartedReader{Reader: reader, entered: entered}, close: reader.Close}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := &HTTPToStdioRelay{URL: "http://owned.fixture", Stdin: input, Stdout: io.Discard, Stderr: io.Discard, HTTPClient: relayInputOnlyClient()}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		if input.closes.Load() != 0 {
			t.Errorf("borrowed Closer treated as authority: %d", input.closes.Load())
		}
		var result error
		early := false
		select {
		case result = <-done:
			early = true
			t.Errorf("borrowed read abandoned/closed: %v", result)
		default:
		}
		_ = writer.CloseWithError(context.Canceled) // only its owner releases it
		if !early {
			result = <-done
		}
		if result != nil {
			t.Fatalf("owner-provided clean cancellation failed: %v", result)
		}
	})
}
